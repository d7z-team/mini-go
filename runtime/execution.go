package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

type ExecutionState string

const (
	ExecutionRunning   ExecutionState = "running"
	ExecutionPending   ExecutionState = "pending"
	ExecutionPaused    ExecutionState = "paused"
	ExecutionCompleted ExecutionState = "completed"
	ExecutionFailed    ExecutionState = "failed"
	ExecutionCanceled  ExecutionState = "canceled"
)

type ExecutionPendingError struct {
	Cause error
}

var executionReady = func() <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}()

func (e ExecutionPendingError) Error() string {
	if e.Cause == nil {
		return "execution is waiting for an external completion"
	}
	return e.Cause.Error()
}

func (e ExecutionPendingError) Unwrap() error { return e.Cause }

type Execution struct {
	mu                 sync.RWMutex
	instance           *Instance
	scopeID            int64
	program            bool
	state              ExecutionState
	result             vmResult
	err                error
	pause              *debugEvent
	debugEpoch         uint64
	nextDebugReference int
	debugInspection    *debugInspection
	profileEvery       uint64
	profileMask        uint64
	profileLimit       int
	profileDropped     uint64
	profileSamples     map[guestSampleKey]uint64
	updates            chan struct{}
	scopeDone          <-chan struct{}
	scopeFinal         ScopeStats
	scopeFinalSet      bool
	cancelRequested    atomic.Bool
}

func newExecution(instance *Instance, options GuestProfileOptions) *Execution {
	execution := &Execution{instance: instance, state: ExecutionRunning, profileEvery: options.SampleEvery, updates: make(chan struct{}, 1)}
	if options.SampleEvery != 0 {
		execution.profileMask = options.SampleEvery - 1
		execution.profileLimit = options.MaxEntries
		if execution.profileLimit == 0 {
			execution.profileLimit = 4096
		}
		execution.profileSamples = make(map[guestSampleKey]uint64)
	}
	return execution
}

func (e *Execution) notify() {
	if e == nil || e.updates == nil {
		return
	}
	select {
	case e.updates <- struct{}{}:
	default:
	}
}

func (e *Execution) State() ExecutionState {
	if e == nil {
		return ExecutionFailed
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// Ready reports when Poll can make progress without periodic polling.
// Running executions are immediately ready; pending executions become ready
// when a timer, FFI completion, interrupt, or debugger event wakes the VM.
func (e *Execution) Ready() <-chan struct{} {
	if e == nil || e.instance == nil || e.instance.vm == nil {
		return nil
	}
	switch e.State() {
	case ExecutionRunning:
		return executionReady
	case ExecutionPending:
		return e.instance.vm.wake
	default:
		return nil
	}
}

func (e *Execution) stateSnapshot() (ExecutionState, vmResult, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state, e.result, e.err
}

func terminalExecutionState(state ExecutionState) bool {
	return state == ExecutionCompleted || state == ExecutionFailed || state == ExecutionCanceled
}

// ErrBusy means a nonblocking Poll could not acquire the VM owner. No
// instructions were executed; callers may retry on a later scheduling turn.
var ErrBusy = errors.New("runtime is busy")

func (e *Execution) Poll() (ExecutionState, error) {
	state, _, err := e.PollSteps(defaultPollQuantum)
	return state, err
}

// PollSteps drives at most maxSteps bytecode instructions and returns the
// number executed by this slice. Runtime-wide and scope limits remain
// cumulative across calls.
func (e *Execution) PollSteps(maxSteps int) (ExecutionState, int, error) {
	return e.pollSteps(context.Background(), maxSteps, false)
}

func (e *Execution) pollSteps(ctx context.Context, maxSteps int, waitForOwner bool) (ExecutionState, int, error) {
	if e == nil || e.instance == nil || e.instance.vm == nil {
		return ExecutionFailed, 0, errors.New("invalid execution")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if maxSteps <= 0 {
		return e.State(), 0, errors.New("execution poll step budget must be positive")
	}
	state, _, executionErr := e.stateSnapshot()
	if state != ExecutionRunning && state != ExecutionPending {
		return state, 0, executionErr
	}
	outcome := e.instance.driveTasks(ctx, maxSteps, waitForOwner)
	if outcome.err != nil && errors.Is(outcome.err, ErrBusy) {
		return state, 0, outcome.err
	}
	e.capture(outcome)
	state, _, executionErr = e.stateSnapshot()
	return state, outcome.executed, executionErr
}

func (e *Execution) waitValues(ctx context.Context) (vmResult, error) {
	if e == nil || e.instance == nil {
		return vmResult{}, errors.New("invalid execution")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopCancel := context.AfterFunc(ctx, e.requestCancel)
	defer stopCancel()
	for {
		state, result, executionErr := e.stateSnapshot()
		switch state {
		case ExecutionCompleted, ExecutionFailed, ExecutionCanceled:
			return result, executionErr
		case ExecutionPaused:
			return vmResult{}, errors.New("execution is paused")
		case ExecutionPending:
			select {
			case <-ctx.Done():
				e.Cancel()
				return vmResult{}, ctx.Err()
			case <-e.instance.vm.wake:
				_, _, _ = e.pollSteps(ctx, defaultPollQuantum, true)
			}
		case ExecutionRunning:
			_, _, pollErr := e.pollSteps(ctx, defaultPollQuantum, true)
			if pollErr != nil && errors.Is(pollErr, ctx.Err()) {
				e.Cancel()
				return vmResult{}, ctx.Err()
			}
		default:
			return vmResult{}, fmt.Errorf("invalid execution state %q", state)
		}
	}
}

func (e *Execution) Wait(ctx context.Context) (RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := e.waitValues(ctx)
	if err != nil {
		return RunResult{}, err
	}
	return hostResultContext(ctx, result, e.instance.vm.limits)
}

func (e *Execution) Cancel() {
	if e == nil || e.instance == nil {
		return
	}
	e.cancelRequested.Store(true)
	// This caller performs the control transition itself. Waking a second
	// driver would leave an obsolete owner request after Cancel has returned.
	e.instance.vm.signalPollWake()
	if err := e.instance.vm.enterOwnerContext(context.Background()); err != nil {
		return
	}
	defer e.instance.vm.leaveOwner()
	e.instance.applyRequestedCancellationsLocked()
	e.instance.vm.sweepRetiredRevisions()
}

func (e *Execution) requestCancel() {
	if e == nil || e.instance == nil || e.instance.vm == nil {
		return
	}
	e.cancelRequested.Store(true)
	e.instance.vm.signalWake()
}

func (e *Execution) cancelOwned() {
	e.mu.Lock()
	if !terminalExecutionState(e.state) {
		e.state = ExecutionCanceled
		e.err = context.Canceled
		e.pause = nil
		e.debugInspection = nil
	}
	e.mu.Unlock()
}

type InterruptHandle struct {
	execution *Execution
}

func (e *Execution) InterruptHandle() InterruptHandle {
	if e == nil || e.instance == nil {
		return InterruptHandle{}
	}
	return InterruptHandle{execution: e}
}

func (h InterruptHandle) Interrupt() {
	if h.execution == nil {
		return
	}
	h.execution.requestCancel()
}

func (e *Execution) resultValues() (vmResult, error) {
	if e == nil {
		return vmResult{}, errors.New("nil execution")
	}
	state, result, executionErr := e.stateSnapshot()
	if !terminalExecutionState(state) {
		return vmResult{}, fmt.Errorf("execution is %s", state)
	}
	return result, executionErr
}

func (e *Execution) Result() (RunResult, error) {
	result, err := e.resultValues()
	if err != nil {
		return RunResult{}, err
	}
	return hostResult(result, e.instance.vm.limits)
}

func (e *Execution) Err() error {
	if e == nil {
		return errors.New("nil execution")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.err
}

func (e *Execution) capture(outcome runOutcome) {
	e.mu.Lock()
	// A synchronous control transition (for example cancellation while a
	// driver is acquiring full ownership) may settle the execution before the
	// driver's observational outcome is returned. That later pending/running
	// observation must not resurrect a terminal handle.
	if terminalExecutionState(e.state) {
		e.mu.Unlock()
		return
	}
	e.result, e.err, e.pause = outcome.result, outcome.err, outcome.pause
	if outcome.state != ExecutionCompleted || e.state != ExecutionCanceled {
		e.state = outcome.state
	}
	if e.state == ExecutionPaused {
		e.buildDebugInspectionLocked()
	}
	if e.state == ExecutionCompleted || e.state == ExecutionFailed || e.state == ExecutionCanceled {
		e.debugInspection = nil
		terminalState := e.state
		failed := terminalState == ExecutionFailed
		scopeFailure := failed && !e.program && isScopePolicyError(e.err)
		program := e.program
		executionErr := e.err
		e.mu.Unlock()
		e.instance.active.CompareAndSwap(e, nil)
		if failed && !scopeFailure {
			e.instance.fail(executionErr)
		} else if program && (terminalState == ExecutionCompleted || terminalState == ExecutionCanceled) {
			e.instance.lifecycle.CompareAndSwap(uint32(instanceOpen), uint32(instanceClosed))
			e.instance.finish(executionErr)
		}
		e.instance.vm.sweepRetiredRevisions()
		return
	}
	e.mu.Unlock()
}
