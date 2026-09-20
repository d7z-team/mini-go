package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

// taskBatch is one owner-selected group of independent task quanta. The owner
// publishes all execution leases at once and does not wait on a pool worker;
// the final worker merely wakes whichever external or supervisor driver will
// merge the results.
type taskBatch struct {
	instance *Instance
	machine  *executionMachine
	runs     []taskRun
	pending  atomic.Int64
	done     chan struct{}
	doneOnce sync.Once
}

type taskRun struct {
	task   *executionTask
	slice  taskSlice
	yield  taskYield
	values []vmValue
	err    error
}

func (machine *executionMachine) prepareTaskBatch(parallelism, budget int) (*taskBatch, runOutcome) {
	if parallelism < 1 {
		parallelism = 1
	}
	if budget <= 0 {
		return nil, failedRun(errors.New("execution poll step budget must be positive"))
	}
	if machine.paused != nil {
		machine.pushRunnableFront(machine.paused)
		machine.paused = nil
	}
	if err := machine.wakeBlocked(); err != nil {
		return nil, failedRun(err)
	}
	count := min(parallelism, budget, machine.runnableCount())
	if count == 0 {
		return nil, machine.idleOutcome()
	}
	runs := make([]taskRun, 0, count)
	remaining := budget
	for len(runs) < count && machine.runnableCount() != 0 {
		task := machine.popRunnable()
		if task == nil || !task.ownership.acquire() {
			continue
		}
		left := count - len(runs)
		limit := remaining / left
		if remaining%left != 0 {
			limit++
		}
		limit = min(limit, taskInstructionQuantum)
		runs = append(runs, taskRun{task: task, slice: taskSlice{limit: limit}})
		remaining -= limit
	}
	if len(runs) == 0 {
		return nil, machine.idleOutcome()
	}
	return &taskBatch{machine: machine, runs: runs, done: make(chan struct{})}, runOutcome{state: ExecutionRunning}
}

func (machine *executionMachine) idleOutcome() runOutcome {
	if machine.runnableCount() != 0 {
		return runOutcome{state: ExecutionRunning}
	}
	if len(machine.blocked) != 0 {
		if len(machine.tasks) != len(machine.blocked) || len(machine.vm.timers) != 0 {
			return runOutcome{state: ExecutionPending}
		}
		for _, task := range machine.blocked {
			if task.blocked != nil && task.blocked.kind == "ffi" {
				return runOutcome{state: ExecutionPending}
			}
		}
		if machine.foreground == nil {
			return runOutcome{state: ExecutionPending}
		}
		return failedRun(machine.allBlockedError())
	}
	if machine.foreground == nil {
		return runOutcome{state: ExecutionPending}
	}
	return failedRun(errors.New("root execution context did not complete"))
}

func (batch *taskBatch) launch() {
	batch.pending.Store(int64(len(batch.runs)))
	executor := defaultExecutor()
	if batch.instance.executor != nil {
		executor = batch.instance.executor
	}
	observer := batch.instance.vm.taskObserver
	var taskIDs []int64
	if observer != nil {
		taskIDs = make([]int64, len(batch.runs))
		for index := range batch.runs {
			taskIDs[index] = batch.runs[index].task.id
		}
	}
	for index := range batch.runs {
		index := index
		job := executor.pool.job(func() bool {
			run := &batch.runs[index]
			if observer != nil {
				observer(run.task.id, true, taskIDs)
				defer observer(run.task.id, false, taskIDs)
			}
			run.yield, run.values, run.err = batch.machine.runTask(run.task, &run.slice)
			run.task.sliceValues = run.values
			run.task.ownership.relinquish()
			batch.instance.vm.releaseSlice()
			if batch.pending.Add(-1) == 0 {
				batch.doneOnce.Do(func() { close(batch.done) })
				batch.instance.vm.signalPollWake()
				batch.instance.signalSupervisor()
			}
			return false
		})
		job.wake()
	}
}

func (machine *executionMachine) mergeTaskBatch(batch *taskBatch) runOutcome {
	outcome := runOutcome{state: ExecutionRunning}
	executed := 0
	for index := range batch.runs {
		run := &batch.runs[index]
		executed += run.slice.executed
		current := machine.applyTaskRunResult(run)
		if current.state == ExecutionFailed || current.state == ExecutionCanceled {
			outcome = current
			continue
		}
		if outcome.state != ExecutionFailed && outcome.state != ExecutionCanceled {
			switch current.state {
			case ExecutionCompleted, ExecutionPaused:
				outcome = current
			case ExecutionPending:
				if outcome.state == ExecutionRunning {
					outcome.state = ExecutionPending
				}
			}
		}
	}
	// A root may complete in the same batch that finishes a module
	// initializer, channel rendezvous or other wait condition for derived
	// tasks. Deliver those handoffs before publishing the root outcome so the
	// background supervisor observes runnable work instead of sleeping with a
	// ready task stranded in the blocked queue.
	if outcome.state != ExecutionFailed && outcome.state != ExecutionCanceled {
		if err := machine.wakeBlocked(); err != nil {
			outcome = failedRun(err)
		} else if outcome.state == ExecutionRunning && machine.runnableCount() == 0 {
			outcome = machine.idleOutcome()
		}
	}
	outcome.executed = executed
	return outcome
}

func (machine *executionMachine) runControlInstruction(task *executionTask, current *executionFrame, pc int, inst *preparedInstruction) (taskYield, bool, error) {
	machine.controlMu.Lock()
	defer machine.controlMu.Unlock()
	yield, handled, err := machine.executeControl(task, current, pc, inst)
	if err != nil {
		if census, ok := findGuestCensusRequest(err); ok {
			current.frame.pc = pc
			task.retryInstruction = true
			task.pendingCensus = census.bytes
			return taskYield{kind: taskYieldCensus}, true, nil
		}
		var guestFault *guestPanic
		if errors.As(err, &guestFault) {
			machine.startPanic(task, current, pc, guestFault.value)
			return taskYield{}, true, nil
		}
		return taskYield{}, handled, machine.vm.runtimeInstructionError(current.frame, current.frame.function.Decl.ID, pc, inst, err)
	}
	if handled && yield.kind == "" && task.blocked != nil {
		machine.blockTask(task, current, pc, inst, task.blocked)
		return taskYield{kind: taskYieldBlocked}, true, nil
	}
	return yield, handled, nil
}

func instructionNeedsControlLock(inst *preparedInstruction) bool {
	if inst == nil {
		return false
	}
	switch inst.op {
	case preparedWaitableTryRecv, preparedWaitableCanRecv, preparedWaitableTrySend,
		preparedWaitableCanSend, preparedWaitableClose:
		return true
	case preparedCallIntrinsic:
		if inst.callIntrinsic == nil {
			return false
		}
		id := string(inst.callIntrinsic.ID)
		return strings.HasPrefix(id, "sync.") || strings.HasPrefix(id, "reflect.") ||
			id == "time.timer_start" || id == "time.timer_stop"
	default:
		return false
	}
}

func (machine *executionMachine) runDataInstruction(task *executionTask, current *executionFrame, pc int, inst *preparedInstruction) (taskYield, error) {
	if instructionNeedsControlLock(inst) {
		machine.controlMu.Lock()
		defer machine.controlMu.Unlock()
	}
	callFrame := current.frame
	functionID := callFrame.function.Decl.ID
	if err := machine.vm.executeInstruction(task, callFrame, inst); err != nil {
		if census, ok := findGuestCensusRequest(err); ok {
			callFrame.pc = pc
			task.retryInstruction = true
			task.pendingCensus = census.bytes
			return taskYield{kind: taskYieldCensus}, nil
		}
		var lock *mutexLockRequest
		if errors.As(err, &lock) {
			machine.blockTask(task, current, pc, inst, &blockedOperation{kind: "mutex", mutex: lock})
			return taskYield{kind: taskYieldBlocked}, nil
		}
		var selectRequest *reflectSelectRequest
		if errors.As(err, &selectRequest) {
			machine.blockTask(task, current, pc, inst, &blockedOperation{kind: "select", selection: selectRequest.selection, reflectSelect: selectRequest})
			return taskYield{kind: taskYieldBlocked}, nil
		}
		var send *reflectSendRequest
		var recv *reflectRecvRequest
		if errors.As(err, &send) || errors.As(err, &recv) {
			selected := channelSelectCase{}
			if send != nil {
				selected.channel, selected.value, selected.send = send.waitable, send.value, true
			} else {
				selected.channel = recv.waitable
			}
			selection, selectErr := machine.vm.prepareChannelSelection(task, callFrame.module, []channelSelectCase{selected})
			if selectErr != nil {
				return taskYield{}, machine.vm.runtimeInstructionError(callFrame, functionID, pc, inst, selectErr)
			}
			if !selection.tryCommit() {
				selection.register()
				machine.blockTask(task, current, pc, inst, &blockedOperation{kind: "select", selection: selection, reflectSend: send != nil, reflectRecv: recv})
				return taskYield{kind: taskYieldBlocked}, nil
			}
			if selection.err != nil {
				machine.startPanic(task, current, pc, newVMValue("String", selection.err.Error()))
				return taskYield{}, nil
			}
			if send != nil {
				callFrame.push(newVMValue("String", ""))
				callFrame.push(newVMValue("Bool", true))
			} else {
				values, receiveErr := recv.complete(selection.value, selection.ok)
				if receiveErr != nil {
					return taskYield{}, machine.vm.runtimeInstructionError(callFrame, functionID, pc, inst, receiveErr)
				}
				for _, value := range values {
					callFrame.push(value)
				}
			}
			return taskYield{}, nil
		}
		var request *artifactCallbackRequest
		if errors.As(err, &request) {
			callee, frameErr := machine.vm.newExecutionFrame(request.module, request.functionID, request.args, request.upvalues, task.id, request.resultCount, false)
			if frameErr != nil {
				return taskYield{}, machine.vm.runtimeInstructionError(callFrame, functionID, pc, inst, frameErr)
			}
			callee.resume = machine.reflectCallCompletion(request.result, request.resumeResults, "artifact callback "+request.functionID+" resumed with")
			task.frames = append(task.frames, callee)
			return taskYield{}, nil
		}
		var guestFault *guestPanic
		if errors.As(err, &guestFault) {
			machine.startPanic(task, current, pc, guestFault.value)
			return taskYield{}, nil
		}
		return taskYield{}, machine.vm.runtimeInstructionError(callFrame, functionID, pc, inst, err)
	}
	if len(callFrame.stack) != 0 && (inst.op == preparedBinary || inst.op == preparedConvert || inst.op == preparedCallIntrinsic) {
		if err := machine.vm.validateRuntimeValue(callFrame.stack[len(callFrame.stack)-1]); err != nil {
			return taskYield{}, machine.vm.runtimeInstructionError(callFrame, functionID, pc, inst, err)
		}
	}
	return taskYield{}, nil
}

// driveTasks is called by an external execution handle. It may wait for task
// slices, but it never occupies an executor worker while doing so.
func (i *Instance) driveTasks(ctx context.Context, budget int, waitForOwner bool) runOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	total := 0
	for total < budget {
		i.driveMu.Lock()
		batch := i.batch
		if batch == nil {
			var err error
			if waitForOwner {
				err = i.vm.enterOwnerContext(ctx)
			} else {
				err = i.vm.enterOwner()
			}
			if err != nil {
				i.driveMu.Unlock()
				return runOutcome{state: ExecutionRunning, executed: total, err: err}
			}
			i.applyRequestedCancellationsLocked()
			if i.vm.machine == nil {
				i.vm.leaveOwner()
				i.driveMu.Unlock()
				return runOutcome{state: ExecutionPending, executed: total}
			}
			var immediate runOutcome
			batch, immediate = i.vm.machine.prepareTaskBatch(i.parallelism, budget-total)
			if batch == nil {
				immediate = i.vm.finishPreparedOutcome(immediate)
				i.vm.leaveOwner()
				i.driveMu.Unlock()
				immediate.executed = total
				return immediate
			}
			batch.instance = i
			i.batch = batch
			i.vm.publishSlices(len(batch.runs))
			batch.launch()
		}
		// The external driver may wait, but an executor worker never does. Keep
		// the drive lease while waiting so the background supervisor cannot
		// consume or replace this caller's batch.
		select {
		case <-batch.done:
		case <-ctx.Done():
			i.driveMu.Unlock()
			return runOutcome{state: ExecutionCanceled, executed: total, err: ctx.Err()}
		}
		if err := i.vm.enterOwnerContext(ctx); err != nil {
			i.driveMu.Unlock()
			return runOutcome{state: ExecutionCanceled, executed: total, err: err}
		}
		outcome := batch.machine.mergeTaskBatch(batch)
		i.batch = nil
		outcome = i.vm.finishPreparedOutcome(outcome)
		i.vm.leaveOwner()
		i.driveMu.Unlock()
		executed := outcome.executed
		total += executed
		outcome.executed = total
		// A census or another resumable control transition may complete without
		// retiring a bytecode instruction. Return ownership after one such batch
		// so an accidental repeated transition cannot trap a host PollSteps call
		// in an unbounded internal loop.
		if outcome.state != ExecutionRunning || total >= budget || executed == 0 {
			return outcome
		}
	}
	return runOutcome{state: ExecutionRunning, executed: total}
}
