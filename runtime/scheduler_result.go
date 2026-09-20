package runtime

import (
	"errors"
	"math"
)

// applyTaskRunResult is the single owner-side transition from an executed
// task slice back into scheduler state. Serial and parallel drivers must use
// the same completion, cancellation, blocking and resource-limit semantics.
func (machine *executionMachine) applyTaskRunResult(run *taskRun) runOutcome {
	task := run.task
	defer func() { task.sliceValues = nil }()
	machine.vm.executedSteps += min(int64(run.slice.executed), math.MaxInt64-machine.vm.executedSteps)
	if task.ownership.terminal() {
		return runOutcome{state: ExecutionRunning}
	}
	if run.err != nil {
		scopeCanceled := task.scope != nil && task.execution != nil && task.execution.cancelRequested.Load()
		scopeLimited := task.scope != nil && !task.scope.program && isScopePolicyError(run.err)
		if scopeCanceled || scopeLimited {
			scope := task.scope
			foreground := machine.foreground == scope
			machine.abortTask(task)
			machine.finishTask(task, run.err)
			machine.cancelScope(scope, run.err)
			if foreground {
				return failedRun(run.err)
			}
			return runOutcome{state: ExecutionRunning}
		}
		machine.abortTask(task)
		machine.finishTask(task, run.err)
		machine.abortTasks(run.err)
		return failedRun(run.err)
	}
	if run.yield.kind == taskYieldPoll || run.yield.kind == taskYieldBudget {
		machine.pushRunnableFront(task)
		if run.yield.kind == taskYieldBudget {
			return runOutcome{state: ExecutionPending}
		}
		return runOutcome{state: ExecutionRunning}
	}
	if run.yield.kind == taskYieldCensus {
		requested := task.pendingCensus
		task.pendingCensus = 0
		live := machine.vm.refreshLiveGuestBytes()
		limit := machine.vm.limits.MaxAllocatedBytes
		if limit == 0 {
			limit = defaultLimits.MaxAllocatedBytes
		}
		if requested > limit-live {
			task.retryInstruction = false
			if len(task.frames) != 0 {
				task.frames[len(task.frames)-1].frame.releasePopValues()
			}
			task.pendingErr = allocationLimitError(limit)
		}
		machine.pushRunnableFront(task)
		return runOutcome{state: ExecutionRunning}
	}
	if run.yield.kind != taskYieldPause {
		task.quantumSteps = 0
	}
	switch run.yield.kind {
	case taskYieldComplete:
		root := machine.foreground != nil && task.id == machine.foreground.rootID
		program := root && machine.foreground.program
		machine.finishTask(task, nil)
		if root {
			if program {
				machine.abortTasks(nil)
			}
			return runOutcome{state: ExecutionCompleted, result: vmResult{Values: run.values}}
		}
	case taskYieldSpawn:
		if err := machine.admitTask(); err != nil {
			machine.abortTask(run.yield.child)
			machine.abortTask(task)
			machine.finishTask(task, err)
			machine.abortTasks(err)
			return failedRun(err)
		}
		machine.addTask(run.yield.child)
		machine.pushRunnable(run.yield.child, task)
	case taskYieldBlocked:
		machine.blocked = append(machine.blocked, task)
	case taskYieldPause:
		machine.paused = task
		event := machine.vm.pausedEvent()
		return runOutcome{state: ExecutionPaused, pause: &event}
	case taskYieldCooperate:
		machine.pushRunnable(task)
	default:
		return failedRun(errors.New("unknown task yield " + run.yield.kind))
	}
	return runOutcome{state: ExecutionRunning}
}
