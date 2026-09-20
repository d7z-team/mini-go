package runtime

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/d7z-team/mini-go/ffi"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type vm struct {
	leaseMu                sync.Mutex
	activeSlices           int
	owner                  atomic.Bool
	controlWaiters         atomic.Int64
	instance               *Instance
	revision               atomic.Pointer[instanceRevision]
	limits                 Limits
	debugger               *Debugger
	breakpointsActive      atomic.Bool
	maxSteps               int64
	nextRunID              int64
	nextExecutionContextID int64
	controlRoots           []vmValue // Temporary values owned by a resource control transaction.
	paused                 *debugPauseState
	wake                   chan struct{}
	ownerWake              chan struct{}
	skipDebugPause         debugResumePoint
	debugStep              debugStepState
	debugStepActive        atomic.Bool
	hostPauseRequested     atomic.Bool
	interruptRequested     atomic.Bool
	pendingEvents          atomic.Int64
	selectState            uint64
	machine                *executionMachine
	liveGuestBytes         atomic.Int64
	allocatedSinceSweep    atomic.Int64
	totalAllocatedBytes    atomic.Int64
	peakGuestBytes         atomic.Int64
	idleFrameBytes         atomic.Int64
	pendingBoundaryBytes   atomic.Int64
	executedSteps          int64
	ffiSession             ffi.Session
	ffiCalls               map[*pendingFFICall]struct{}
	retiredMu              sync.Mutex
	retiredRevisions       []*instanceRevision
	reflectTypes           metadataCache[string, TypeInfo]
	reflectTypeValues      metadataCache[string, vmValue]
	dynamicTypeCount       int
	dynamicTypeBytes       int64
	hostMu                 sync.Mutex
	clock                  Clock
	entropy                io.Reader
	timers                 map[*waitableResource]*runtimeTimer
	profileOptions         GuestProfileOptions
	taskObserver           func(int64, bool, []int64)
}

const (
	moduleInitFunctionID   = "fn.init"
	programEntryFunctionID = "fn.main"
)

type vmResult struct {
	Values []vmValue
}

type Error struct {
	Generation         uint64
	ProgramHash        string
	ModulePath         string
	FunctionID         string
	PC                 int
	Op                 string
	RouteID            string
	ExecutionContextID int64
	Loc                *ir.Location
	Err                error
	stack              []runtimeStackFrame
}

type runtimeStackFrame struct {
	Revision           RevisionInfo
	ExecutionContextID int64
	ModulePath         string
	FunctionID         string
	PC                 int
	Loc                *ir.Location
}

func (e Error) Error() string {
	if e.FunctionID == "" {
		if e.Err == nil {
			return "runtime error"
		}
		return e.Err.Error()
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.FunctionID, e.Err)
	}
	if e.Loc != nil {
		return fmt.Sprintf("%s instruction %d %s @%s:%d:%d: %v", e.FunctionID, e.PC, e.Op, e.Loc.File, e.Loc.Line, e.Loc.Column, e.Err)
	}
	return fmt.Sprintf("%s instruction %d %s: %v", e.FunctionID, e.PC, e.Op, e.Err)
}

func (e Error) Unwrap() error {
	return e.Err
}

func (e Error) Location() (ir.Location, bool) {
	if e.Loc == nil {
		return ir.Location{}, false
	}
	return *e.Loc, true
}

type panicError struct {
	Value       vmValue
	Generation  uint64
	ProgramHash string
	ModulePath  string
	FunctionID  string
	PC          int
	Loc         *ir.Location
}

func (e panicError) Error() string {
	return fmt.Sprintf("panic: %s(%v)", e.Value.Type, e.Value.Data)
}

type StepLimitError struct {
	MaxSteps int64
}

type ResourceLimitError struct {
	Code    string
	Message string
}

func (e ResourceLimitError) Error() string { return e.Message }

func (e StepLimitError) Error() string {
	return fmt.Sprintf("execution step limit exceeded: max %d", e.MaxSteps)
}

func isScopePolicyError(err error) bool {
	var stepLimit StepLimitError
	if errors.As(err, &stepLimit) {
		return true
	}
	var resourceLimit ResourceLimitError
	return errors.As(err, &resourceLimit)
}

// BlockedContext describes one task that cannot make internal progress.
type BlockedContext struct {
	ScopeID            int64
	ExecutionContextID int64
	Revision           RevisionInfo
	ModulePath         string
	FunctionID         string
	PC                 int
	Op                 string
	Loc                *ir.Location
	Reason             string
}

// AllBlockedError reports a scheduler deadlock after external progress sources
// have been excluded. Contexts is deterministic and bounded; Total is exact.
type AllBlockedError struct {
	Total    int
	Contexts []BlockedContext
}

func (e AllBlockedError) Error() string {
	if len(e.Contexts) == 0 {
		return "all VM execution contexts are blocked"
	}
	first := e.Contexts[0]
	reason := first.Reason
	if reason == "" {
		reason = "execution blocked"
	}
	return fmt.Sprintf("all %d VM execution contexts are blocked; first is context %d %s instruction %d %s: %s", e.Total, first.ExecutionContextID, first.FunctionID, first.PC, first.Op, reason)
}

var errNilFunctionCall = errors.New("nil function call")

// InstanceOptions configures one isolated runtime instance.
type InstanceOptions struct {
	FFI     ffi.Bridge
	Clock   Clock
	Entropy io.Reader
	Limits  Limits
	// Parallelism bounds simultaneously running guest tasks in this instance.
	// Zero selects one worker. Tasks share the process-wide bounded executor;
	// the instance never creates a goroutine per guest task.
	Parallelism  int
	Executor     *Executor
	Debugger     *Debugger
	GuestProfile GuestProfileOptions
	taskObserver func(int64, bool, []int64)
	modules      *moduleRegistry
	ffiSession   ffi.Session
}

// GuestProfileOptions enables bounded instruction sampling for one execution.
// A zero SampleEvery disables profiling without adding work to ordinary runs.
type GuestProfileOptions struct {
	SampleEvery uint64
	MaxEntries  int
}

type Limits struct {
	// MaxSteps is the shared scope instruction budget: 0 defaults to 100 million,
	// UnlimitedSteps disables it, and positive values set a finite budget.
	MaxSteps              int64
	MaxCallDepth          int
	MaxTasks              int
	MaxAllocatedBytes     int64
	MaxStringBytes        int
	MaxCollectionElements int
	MaxPendingEvents      int
	MaxBoundaryDepth      int
	MaxBoundaryBytes      int64
	MaxRetainedRevisions  int
	// Dynamic reflection metadata is retained for the Instance lifetime.
	MaxDynamicTypes     int
	MaxDynamicTypeBytes int64
}

func newVMWithOptions(executable *executable, options InstanceOptions) (*vm, error) {
	if executable == nil {
		return nil, errors.New("nil executable")
	}
	if interval := options.GuestProfile.SampleEvery; interval != 0 && interval&(interval-1) != 0 {
		return nil, errors.New("guest profile sample interval must be a power of two")
	}
	if options.GuestProfile.MaxEntries < 0 {
		return nil, errors.New("guest profile entry limit cannot be negative")
	}
	if options.Parallelism < 0 {
		return nil, errors.New("instance parallelism cannot be negative")
	}
	modules := options.modules.clone()
	root := newModuleInstance(executable)
	rootPath := strings.TrimSpace(executable.Artifact.Module.Path)
	if rootPath != "" {
		if modules.modules == nil {
			modules.modules = make(map[string]*moduleInstance)
		}
		if existing, exists := modules.modules[rootPath]; exists && existing != root {
			return nil, fmt.Errorf("module registry already contains root module %q", rootPath)
		}
		if err := modules.addModule(root); err != nil {
			return nil, err
		}
	} else {
		root.registry = modules
	}
	if err := bindModuleRequirements(root, modules); err != nil {
		return nil, err
	}
	limits := normalizeLimits(options.Limits)
	clock := options.Clock
	if clock == nil {
		clock = systemClock{}
	}
	entropy := options.Entropy
	if entropy == nil {
		entropy = cryptorand.Reader
	}
	machine := &vm{
		limits:         limits,
		profileOptions: options.GuestProfile,
		taskObserver:   options.taskObserver,
		debugger:       options.Debugger,
		ffiSession:     options.ffiSession,
		maxSteps:       limits.MaxSteps,
		wake:           make(chan struct{}, 1),
		ownerWake:      make(chan struct{}, 1),
		clock:          clock,
		entropy:        entropy,
		timers:         make(map[*waitableResource]*runtimeTimer),
	}
	if machine.debugger == nil {
		machine.debugger = NewDebugger()
	}
	machine.debugger.mu.Lock()
	if machine.debugger.bound || machine.debugger.closed {
		machine.debugger.mu.Unlock()
		return nil, errors.New("debugger is already bound or closed")
	}
	machine.debugger.bound = true
	machine.debugger.mu.Unlock()
	machine.breakpointsActive.Store(machine.debugger.hasActiveBreakpoints())
	revision := &instanceRevision{generation: 1, root: root, modules: modules}
	machine.revision.Store(revision)
	for _, module := range modules.modules {
		module.revision = revision
		module.vm = machine
	}
	return machine, nil
}

func (vm *vm) rootModule() *moduleInstance {
	if vm == nil {
		return nil
	}
	revision := vm.revision.Load()
	if revision == nil {
		return nil
	}
	return revision.root
}

func (vm *vm) moduleRegistry() *moduleRegistry {
	if vm == nil {
		return nil
	}
	revision := vm.revision.Load()
	if revision == nil {
		return nil
	}
	return revision.modules
}

func (vm *vm) signalWake() {
	if vm == nil || vm.wake == nil {
		return
	}
	vm.signalPollWake()
	if vm.instance != nil {
		vm.instance.signalSupervisor()
	}
}

func (vm *vm) signalPollWake() {
	if vm == nil || vm.wake == nil {
		return
	}
	select {
	case vm.wake <- struct{}{}:
	default:
	}
}

func (vm *vm) prepareFunction(functionID string, args []vmValue) (int64, error) {
	if vm == nil {
		return 0, errors.New("nil VM")
	}
	if vm.paused != nil || vm.machine != nil && vm.machine.foreground != nil {
		return 0, errors.New("VM already has an active execution")
	}
	root := vm.rootModule()
	if root == nil || root.executable == nil {
		return 0, errors.New("nil root module")
	}
	if _, ok := root.executable.Functions[functionID]; !ok {
		return 0, Error{ModulePath: root.modulePath(), Err: fmt.Errorf("unknown function %q", functionID)}
	}
	scopeID := vm.beginRun()
	vm.interruptRequested.Store(false)
	if err := vm.prepareScheduledFunction(root, functionID, args, vm.nextSpawnExecutionContextID(), scopeID, false); err != nil {
		vm.finishForeground()
		return 0, err
	}
	return scopeID, nil
}

func (vm *vm) prepareProgramEntry() (int64, error) {
	if vm == nil {
		return 0, errors.New("nil VM")
	}
	if vm.paused != nil || vm.machine != nil && vm.machine.foreground != nil {
		return 0, errors.New("VM already has an active execution")
	}
	root := vm.rootModule()
	if root == nil || root.executable == nil {
		return 0, errors.New("nil root module")
	}
	artifact := root.executable.Artifact
	if artifact.Module.Package != "main" {
		return 0, fmt.Errorf("module %q package is %q, not main", artifact.Module.Path, artifact.Module.Package)
	}
	entry, ok := root.executable.Functions[programEntryFunctionID]
	if !ok {
		return 0, fmt.Errorf("module %q missing func main", artifact.Module.Path)
	}
	if len(entry.Decl.Signature.Params) != 0 || len(entry.Decl.Signature.Results) != 0 || entry.Decl.Signature.Variadic {
		return 0, fmt.Errorf("module %q func main must have no parameters or results", artifact.Module.Path)
	}
	scopeID := vm.beginRun()
	vm.interruptRequested.Store(false)
	if err := vm.prepareScheduledFunction(root, programEntryFunctionID, nil, vm.nextSpawnExecutionContextID(), scopeID, true); err != nil {
		vm.finishForeground()
		return 0, err
	}
	return scopeID, nil
}

func (vm *vm) runPrepared(instructionBudget int) runOutcome {
	if vm == nil || vm.machine == nil {
		return failedRun(errors.New("VM has no prepared execution"))
	}
	program := vm.machine.foreground != nil && vm.machine.foreground.program
	outcome := vm.machine.run(instructionBudget)
	return vm.finishPreparedOutcomeForProgram(outcome, program)
}

func (vm *vm) finishPreparedOutcome(outcome runOutcome) runOutcome {
	program := vm != nil && vm.machine != nil && vm.machine.foreground != nil && vm.machine.foreground.program
	return vm.finishPreparedOutcomeForProgram(outcome, program)
}

func (vm *vm) finishPreparedOutcomeForProgram(outcome runOutcome, program bool) runOutcome {
	if outcome.suspended() {
		return outcome
	}
	if vm.machine.foreground != nil {
		vm.machine.publishScopeRoot(vm.machine.foreground.id, outcome.state, outcome.err)
	}
	err := outcome.err
	if err != nil {
		if errors.Is(err, context.Canceled) && program {
			vm.finishRun()
		} else if errors.Is(err, context.Canceled) || !program && isScopePolicyError(err) {
			vm.finishForeground()
		} else {
			vm.machine.abortTasks(err)
			vm.finishRun()
		}
		return outcome
	}
	if program {
		vm.finishRun()
	} else {
		vm.finishForeground()
		if vm.machine != nil && vm.machine.runnableCount() != 0 && vm.instance != nil {
			vm.instance.signalSupervisor()
		}
	}
	return outcome
}
