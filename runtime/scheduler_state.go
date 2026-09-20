package runtime

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type deferredCall struct {
	caller *moduleInstance
	ref    functionRef
}

type executionMachine struct {
	// controlMu protects the small shared scheduler/resource transitions that
	// may be requested by concurrently running task slices. Full owner actions
	// run only after all slices have returned and do not take this lock.
	controlMu    sync.Mutex
	vm           *vm
	foreground   *executionScope
	scopes       map[int64]*executionScope
	tasks        map[int64]*executionTask
	runnable     []*executionTask
	runnableHead int
	blocked      []*executionTask
	paused       *executionTask
}

// taskSlice is owned by one dispatch. The driver merges its counters only
// after the execution lease has returned.
type taskSlice struct {
	limit     int
	attempted int
	executed  int
}

func (machine *executionMachine) runnableCount() int {
	if machine == nil {
		return 0
	}
	return len(machine.runnable) - machine.runnableHead
}

func (machine *executionMachine) runnableTasks() []*executionTask {
	if machine == nil || machine.runnableHead >= len(machine.runnable) {
		return nil
	}
	return machine.runnable[machine.runnableHead:]
}

func (machine *executionMachine) pushRunnable(tasks ...*executionTask) {
	if len(tasks) == 0 {
		return
	}
	active := machine.runnableCount()
	if active == 0 {
		clear(machine.runnable)
		machine.runnable = machine.runnable[:0]
		machine.runnableHead = 0
	} else if machine.runnableHead > 0 && active+len(tasks) <= cap(machine.runnable) {
		copy(machine.runnable, machine.runnable[machine.runnableHead:])
		clear(machine.runnable[active:])
		machine.runnable = machine.runnable[:active]
		machine.runnableHead = 0
	}
	for _, task := range tasks {
		if task.ownership.enqueue() {
			machine.runnable = append(machine.runnable, task)
		}
	}
}

func (machine *executionMachine) pushRunnableFront(task *executionTask) {
	if !task.ownership.enqueue() {
		return
	}
	if machine.runnableHead > 0 {
		machine.runnableHead--
		machine.runnable[machine.runnableHead] = task
		return
	}
	machine.runnable = append(machine.runnable, nil)
	copy(machine.runnable[1:], machine.runnable[:len(machine.runnable)-1])
	machine.runnable[0] = task
}

func (machine *executionMachine) popRunnable() *executionTask {
	if machine.runnableCount() == 0 {
		return nil
	}
	task := machine.runnable[machine.runnableHead]
	machine.runnable[machine.runnableHead] = nil
	machine.runnableHead++
	if machine.runnableHead == len(machine.runnable) {
		machine.runnable = machine.runnable[:0]
		machine.runnableHead = 0
	}
	return task
}

type executionScope struct {
	id            int64
	rootID        int64
	program       bool
	execution     *Execution
	budget        *executionBudget
	started       RevisionInfo
	rootState     ExecutionState
	rootPublished bool
	tasks         int
	timers        int
	ffiCalls      int
	err           error
	done          chan struct{}
	settled       bool
}

type executionTask struct {
	ownership taskOwnership
	// Poll suspension preserves the current scheduling quantum.
	quantumSteps       int
	profilePhase       uint64
	id                 int64
	scope              *executionScope
	execution          *Execution
	budget             *executionBudget
	stepGrant          int64
	stepsLeft          int64
	frames             []*executionFrame
	completingFrame    *executionFrame
	preparingSelection *channelSelection
	retryInstruction   bool
	pendingCensus      int64
	debugParents       []debugFrame
	blocked            *blockedOperation
	pendingErr         error
	// sliceValues roots a completed task's values between publication of the
	// task continuation and owner-side result processing.
	sliceValues []vmValue
}

func (task *executionTask) retainedFrames(yield func(int, *executionFrame) bool) {
	for index, frame := range task.frames {
		if !yield(index, frame) {
			return
		}
	}
	if task.completingFrame != nil {
		yield(len(task.frames), task.completingFrame)
	}
}

func (machine *executionMachine) attachExecution(scopeID int64, execution *Execution) {
	if machine == nil || scopeID == 0 || execution == nil {
		return
	}
	scope := machine.scopes[scopeID]
	if scope == nil {
		return
	}
	scope.execution = execution
	execution.mu.Lock()
	execution.scopeDone = scope.done
	execution.mu.Unlock()
	for _, task := range machine.tasks {
		if task.scope == scope {
			task.execution = execution
		}
	}
}

type executionBudget struct {
	steps        atomic.Int64
	profilePhase atomic.Uint64
	mu           sync.Mutex
	committed    int64
	reserved     int64
	waiting      bool
	wake         func()
}

var errStepBudgetReserved = errors.New("instruction allowance is held by another task")

func (task *executionTask) releaseStepGrant() {
	if task.stepGrant == 0 {
		return
	}
	budget := task.budget
	budget.mu.Lock()
	budget.reserved -= task.stepGrant
	used := task.stepGrant - task.stepsLeft
	budget.committed += min(used, math.MaxInt64-budget.committed)
	waiting := budget.waiting
	budget.waiting = false
	budget.mu.Unlock()
	task.stepGrant, task.stepsLeft = 0, 0
	if waiting && budget.wake != nil {
		budget.wake()
	}
}

func (task *executionTask) consumeStep(limit int64) error {
	if task.budget == nil {
		task.budget = &executionBudget{}
	}
	if task.stepsLeft == 0 {
		task.releaseStepGrant()
		budget := task.budget
		budget.mu.Lock()
		if budget.reserved == 0 {
			budget.committed = budget.steps.Load()
		}
		grant := int64(taskInstructionQuantum)
		if limit > 0 {
			if budget.committed >= limit {
				budget.mu.Unlock()
				return StepLimitError{MaxSteps: limit}
			}
			grant = min(grant, limit-budget.committed-budget.reserved)
			if grant <= 0 {
				budget.waiting = true
				budget.mu.Unlock()
				return errStepBudgetReserved
			}
		}
		budget.reserved += grant
		budget.mu.Unlock()
		task.stepGrant, task.stepsLeft = grant, grant
	}
	task.stepsLeft--
	for {
		steps := task.budget.steps.Load()
		if steps == math.MaxInt64 || task.budget.steps.CompareAndSwap(steps, steps+1) {
			break
		}
	}
	task.profilePhase = task.budget.profilePhase.Add(1)
	return nil
}

type executionFrame struct {
	frame           *frame
	expectedResults int
	qualifyResults  bool
	resume          func(*executionTask, *executionFrame, []vmValue) error
	abort           func()
	deferred        bool
	recoveredPanic  *machinePanic
	completion      *frameCompletion
	pinnedRevision  *instanceRevision
}

type artifactCallbackRequest struct {
	module        *moduleInstance
	functionID    string
	args          []vmValue
	upvalues      map[string]*slot
	resultCount   int
	resumeResults int
	result        reflectCallResult
}

type reflectRecvRequest struct {
	waitable vmValue
	ctx      intrinsicContext
}

type reflectSendRequest struct {
	waitable vmValue
	ctx      intrinsicContext
	value    vmValue
}

type reflectSelectRequest struct {
	ctx       intrinsicContext
	selection *channelSelection
	indexes   []int
}

func (request *reflectRecvRequest) Error() string {
	return "reflect receive blocked"
}

func (request *reflectSendRequest) Error() string {
	return "reflect send blocked"
}

func (request *reflectSelectRequest) Error() string {
	return "reflect select blocked"
}

func (request *artifactCallbackRequest) Error() string {
	if request == nil {
		return "nil artifact callback request"
	}
	return "artifact callback request for " + request.functionID
}

type frameCompletion struct {
	returnValues []vmValue
	panic        *machinePanic
	resume       bool
}

type machinePanic struct {
	err        panicError
	runtimeErr Error
	debugEvent *debugEvent
}

type blockedOperation struct {
	selection     *channelSelection
	selectPayload *ir.SelectPayload
	mutex         *mutexLockRequest
	ticket        *taskWaitTicket
	kind          string
	module        *moduleInstance
	moduleReady   func(*executionTask, *executionFrame) error
	ffi           *pendingFFICall
	withOK        bool
	reflectRecv   *reflectRecvRequest
	reflectSend   bool
	reflectSelect *reflectSelectRequest
	error         Error
}

type taskYield struct {
	kind  string
	child *executionTask
}
