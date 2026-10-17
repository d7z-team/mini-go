package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func (machine *executionMachine) debugParentSnapshot(task *executionTask) []debugFrame {
	out := append([]debugFrame(nil), task.debugParents...)
	for _, active := range task.frames {
		frame := active.frame
		if frame == nil || frame.module == nil || frame.module.executable == nil {
			continue
		}
		pc := frame.pc
		if pc > 0 {
			pc--
		}
		functionID := frame.function.Decl.ID
		loc, _ := frame.revision.symbols.nearestLocation(frame.module.modulePath(), functionID, pc)
		out = append(out, debugFrame{
			ScopeID:            task.scope.id,
			SymbolsHash:        frame.revision.symbolsHash(),
			SourceHash:         frame.revision.symbols.sourceHash(frame.module.modulePath(), loc.File),
			Generation:         frame.revisionGeneration(),
			ProgramHash:        frame.revisionHash(),
			hasSymbols:         frame.revision != nil && frame.revision.symbols != nil,
			ExecutionContextID: frame.executionContextID,
			ModulePath:         frame.module.modulePath(),
			FunctionID:         functionID,
			PC:                 pc,
			Loc:                loc,
		})
	}
	return out
}

func (vm *vm) pausedEvent() debugEvent {
	if vm == nil || vm.paused == nil || vm.paused.frame == nil {
		return debugEvent{}
	}
	if vm.debugger != nil {
		return vm.debugger.pausedEvent()
	}
	return debugEvent{}
}

func (machine *executionMachine) module(modulePath string) (*moduleInstance, error) {
	module, ok := machine.vm.moduleRegistry().module(strings.TrimSpace(modulePath))
	if !ok {
		return nil, fmt.Errorf("module %q is not loaded", modulePath)
	}
	return module, nil
}

func (machine *executionMachine) directCallTarget(frame *frame, inst *preparedInstruction) (*moduleInstance, loadedFunction, error) {
	modulePath := inst.callModule
	if modulePath == "" {
		modulePath = strings.TrimSpace(inst.call.ModulePath)
		if modulePath == "" {
			modulePath = frame.module.modulePath()
		}
	}
	if inst.callRevisionLocal {
		if frame == nil || frame.revision == nil || frame.revision.modules == nil {
			return nil, loadedFunction{}, errors.New("revision-local call has no caller revision")
		}
		module, ok := frame.revision.modules.module(modulePath)
		if !ok || module == nil || module.executable == nil {
			return nil, loadedFunction{}, fmt.Errorf("module %q is not loaded in caller revision", modulePath)
		}
		function, ok := module.executable.Functions[inst.call.Function]
		if !ok {
			return nil, loadedFunction{}, fmt.Errorf("unknown function %q", inst.call.Function)
		}
		return module, function, nil
	}
	current := machine.vm.revision.Load()
	if current != nil && frame.revision == current &&
		inst.callModuleIndex >= 0 && inst.callModuleIndex < len(current.moduleOrder) {
		module := current.moduleOrder[inst.callModuleIndex]
		if module != nil && inst.callFunctionIndex >= 0 && inst.callFunctionIndex < len(module.executable.FunctionOrder) {
			return module, module.executable.FunctionOrder[inst.callFunctionIndex], nil
		}
	}
	target, err := machine.module(modulePath)
	if err != nil {
		return nil, loadedFunction{}, err
	}
	function, ok := target.executable.Functions[inst.call.Function]
	if !ok {
		return nil, loadedFunction{}, fmt.Errorf("unknown function %q", inst.call.Function)
	}
	return target, function, nil
}

func (machine *executionMachine) initializeModule(task *executionTask, caller *executionFrame, module *moduleInstance, ready func(*executionTask, *executionFrame) error) error {
	if module == nil || module.executable == nil {
		return errors.New("nil module")
	}
	if module.state.initState == moduleReady {
		if ready == nil {
			return nil
		}
		return ready(task, caller)
	}
	modulePath := module.executable.Artifact.Module.Path
	if module.state.initState == moduleFailed {
		return module.state.initErr
	}
	if module.state.initState == moduleInitializing {
		for owner := module.state.initTask; owner != nil; {
			if owner == task {
				return fmt.Errorf("module %q initialization cycle", modulePath)
			}
			if owner.blocked == nil || owner.blocked.module == nil {
				break
			}
			owner = owner.blocked.module.state.initTask
		}
		if caller == nil {
			return fmt.Errorf("module %q initialization has no continuation", modulePath)
		}
		task.blocked = &blockedOperation{kind: "module", module: module, moduleReady: ready}
		return nil
	}
	if _, ok := module.executable.Functions[moduleInitFunctionID]; !ok {
		module.state.initState = moduleReady
		if ready == nil {
			return nil
		}
		return ready(task, caller)
	}
	initFrame, err := machine.vm.newExecutionFrame(module, moduleInitFunctionID, nil, nil, task.id, 0, false, 0)
	if err != nil {
		return err
	}
	// Do not publish the initializing state until frame preparation succeeds;
	// failed admission must leave module initialization retryable.
	module.state.beginInitialization()
	module.state.initTask = task
	initFrame.abort = func() { module.state.finishInitialization(errors.New("module initialization aborted")) }
	initFrame.resume = func(task *executionTask, caller *executionFrame, _ []vmValue) error {
		module.state.finishInitialization(nil)
		initFrame.abort = nil
		if ready == nil {
			return nil
		}
		return ready(task, caller)
	}
	task.frames = append(task.frames, initFrame)
	return nil
}

func (machine *executionMachine) admitTask() error {
	active := len(machine.tasks)
	if limit := machine.vm.limits.MaxTasks; limit > 0 && active >= limit {
		return ResourceLimitError{Code: "execution.task_limit", Message: fmt.Sprintf("execution task limit exceeded: max %d", limit)}
	}
	return nil
}

const (
	taskYieldComplete  = "complete"
	taskYieldSpawn     = "spawn"
	taskYieldBlocked   = "blocked"
	taskYieldPause     = "pause"
	taskYieldCooperate = "cooperate"
	taskYieldPoll      = "poll"
	taskYieldBudget    = "budget"
)

func (vm *vm) newExecutionFrame(module *moduleInstance, functionID string, args []vmValue, captureSlots []*slot, contextID int64, expectedResults int, qualifyResults bool, additionalStorage int64, batches ...*managedAllocationBatch) (*executionFrame, error) {
	if module == nil || module.executable == nil {
		return nil, errors.New("nil module")
	}
	fn, ok := module.executable.Functions[functionID]
	if !ok {
		return nil, fmt.Errorf("unknown function %q", functionID)
	}
	return vm.newPreparedExecutionFrame(module, fn, args, captureSlots, contextID, expectedResults, qualifyResults, additionalStorage, batches...)
}

func (vm *vm) newPreparedExecutionFrame(module *moduleInstance, fn loadedFunction, args []vmValue, captureSlots []*slot, contextID int64, expectedResults int, qualifyResults bool, additionalStorage int64, batches ...*managedAllocationBatch) (*executionFrame, error) {
	return vm.newPreparedExecutionFrameWithSource(module, fn, args, nil, captureSlots, contextID, expectedResults, qualifyResults, additionalStorage, batches...)
}

func (vm *vm) newPreparedExecutionFrameFromOperands(module *moduleInstance, fn loadedFunction, caller *frame, operands []ir.Operand, captureSlots []*slot, contextID int64, expectedResults int, qualifyResults bool, additionalStorage int64, batches ...*managedAllocationBatch) (*executionFrame, error) {
	return vm.newPreparedExecutionFrameWithSource(module, fn, nil, &frameArgumentSource{caller: caller, operands: operands}, captureSlots, contextID, expectedResults, qualifyResults, additionalStorage, batches...)
}

func (vm *vm) newPreparedExecutionFrameWithSource(module *moduleInstance, fn loadedFunction, args []vmValue, source *frameArgumentSource, captureSlots []*slot, contextID int64, expectedResults int, qualifyResults bool, additionalStorage int64, batches ...*managedAllocationBatch) (*executionFrame, error) {
	argumentCount := len(args)
	if source != nil {
		argumentCount = len(source.operands)
	}
	if argumentCount != len(fn.Decl.Signature.Params) {
		return nil, fmt.Errorf("function %s argument count mismatch: got %d, want %d", fn.Decl.ID, argumentCount, len(fn.Decl.Signature.Params))
	}
	if expectedResults >= 0 && expectedResults != len(fn.Decl.Signature.Results) {
		return nil, fmt.Errorf("function %s result count mismatch: got %d, want %d", fn.Decl.ID, expectedResults, len(fn.Decl.Signature.Results))
	}
	allocationBytes := additionalStorage + preparedFrameLogicalBytes(module, fn, len(captureSlots))
	reservation, err := vm.reserveControlBytes(allocationBytes, batches...)
	if err != nil {
		return nil, err
	}
	var callFrame *frame
	if source != nil {
		callFrame, _, err = newFrameWithArgumentSource(module, fn, nil, source, captureSlots, contextID, batches...)
	} else {
		callFrame, _, err = newFrame(module, fn, args, captureSlots, contextID, batches...)
	}
	if err != nil {
		reservation.abort()
		return nil, err
	}
	reservation.commit()
	callFrame.admittedBytes = allocationBytes
	callFrame.allocationAdmitted = true
	var pinnedRevision *instanceRevision
	if module.revision.retain() {
		pinnedRevision = module.revision
	}
	callFrame.execution = executionFrame{
		frame:           callFrame,
		expectedResults: expectedResults,
		qualifyResults:  qualifyResults,
		pinnedRevision:  pinnedRevision,
	}
	return &callFrame.execution, nil
}

func (vm *vm) prepareScheduledFunction(module *moduleInstance, functionID string, args []vmValue, contextID, scopeID int64, program bool) error {
	machine := vm.machine
	if machine == nil {
		machine = &executionMachine{vm: vm}
		vm.machine = machine
	}
	if machine.foreground != nil {
		return errors.New("VM already has an active execution")
	}
	if err := machine.admitTask(); err != nil {
		return err
	}
	root, err := vm.newExecutionFrame(module, functionID, args, nil, contextID, -1, false, ir.StorageControlRecordBytes)
	if err != nil {
		return Error{
			Generation:  module.revision.generation,
			ProgramHash: module.revision.code.image.Hash,
			ModulePath:  module.modulePath(),
			FunctionID:  functionID,
			Err:         err,
		}
	}
	scope := machine.newScope(scopeID, contextID, program, module.revision)
	task := &executionTask{id: contextID, scope: scope, budget: scope.budget, frames: []*executionFrame{root}}
	machine.foreground = scope
	machine.addTask(task)
	if functionID != moduleInitFunctionID {
		if err := machine.initializeModule(task, root, module, nil); err != nil {
			machine.abortTask(task)
			machine.finishTask(task, err)
			machine.foreground = nil
			delete(machine.scopes, scopeID)
			return err
		}
	}
	machine.pushRunnable(task)
	return nil
}

func (vm *vm) prepareRootInitialization() (int64, bool, error) {
	if vm == nil {
		return 0, false, errors.New("nil VM")
	}
	root := vm.rootModule()
	if root == nil || root.executable == nil {
		return 0, false, errors.New("nil root module")
	}
	if root.state.initState == moduleReady {
		return 0, false, nil
	}
	if _, ok := root.executable.Functions[moduleInitFunctionID]; !ok {
		root.state.initState = moduleReady
		return 0, false, nil
	}
	scopeID := vm.beginRun()
	contextID := vm.nextSpawnExecutionContextID()
	machine := vm.machine
	if machine == nil {
		machine = &executionMachine{vm: vm}
		vm.machine = machine
	}
	if err := machine.admitTask(); err != nil {
		return 0, false, err
	}
	scope := machine.newScope(scopeID, contextID, false, root.revision)
	task := &executionTask{id: contextID, scope: scope, budget: scope.budget}
	machine.foreground = scope
	machine.addTask(task)
	if err := machine.initializeModule(task, nil, root, nil); err != nil {
		machine.abortTask(task)
		machine.finishTask(task, err)
		machine.foreground = nil
		delete(machine.scopes, scopeID)
		vm.finishForeground()
		return 0, false, err
	}
	machine.pushRunnable(task)
	return scopeID, true, nil
}

func (machine *executionMachine) run(instructionBudget int) (outcome runOutcome) {
	attempted := 0
	executed := 0
	defer func() { outcome.executed = executed }()
	if machine.cancelRequestedScopes() != nil {
		return failedRun(context.Canceled)
	}
	if machine.paused != nil {
		machine.pushRunnableFront(machine.paused)
		machine.paused = nil
	}
	for {
		if machine.vm.heap != nil && machine.vm.heap.collectionRequested.Load() {
			if err := machine.vm.collectManagedAtSafepoint(); err != nil {
				return failedRun(err)
			}
		}
		if machine.runnableCount() != 0 {
			if instructionBudget > 0 && attempted >= instructionBudget {
				return runOutcome{state: ExecutionRunning}
			}
			task := machine.popRunnable()
			if !task.ownership.acquire() {
				continue
			}
			slice := taskSlice{}
			if instructionBudget > 0 {
				slice.limit = instructionBudget - attempted
			}
			yield, values, err := machine.runTask(task, &slice)
			task.ownership.relinquish()
			attempted += slice.attempted
			executed += slice.executed
			current := machine.applyTaskRunResult(&taskRun{task: task, slice: slice, yield: yield, values: values, err: err})
			if current.state != ExecutionRunning {
				return current
			}
			if yield.kind == taskYieldPoll {
				return current
			}
			if err := machine.wakeBlocked(); err != nil {
				return failedRun(err)
			}
			continue
		}
		if err := machine.wakeBlocked(); err != nil {
			return failedRun(err)
		}
		if machine.runnableCount() != 0 {
			continue
		}
		return machine.idleOutcome()
	}
}

func (machine *executionMachine) runTask(task *executionTask, slice *taskSlice) (yield taskYield, values []vmValue, err error) {
	defer task.releaseStepGrant()
	defer func() { task.preparingSelection = nil }()
	if task.blocked != nil {
		if task.blocked.ticket == nil {
			task.blocked.ticket = task.ownership.beginWait()
		}
		return taskYield{kind: taskYieldBlocked}, nil, nil
	}
	// The serial owner path does not go through taskBatch.launch, so it must
	// still receive the same worker TLAB as parallel slices. Without this lease
	// a nursery-full page can only report errManagedCollection from the mutator
	// and aggregate values panic instead of reaching the owner safepoint.
	if task.allocationBatch == nil && machine.vm.heap != nil {
		heap := machine.vm.heap
		credit := managedBatchCredit
		heap.mu.Lock()
		used := heap.counters.occupiedBytes + heap.counters.reservedBytes
		if heap.maxGuestBytes != 0 && used < heap.maxGuestBytes && heap.maxGuestBytes-used < credit {
			credit = heap.maxGuestBytes - used
		}
		heap.mu.Unlock()
		var batch *managedAllocationBatch
		// A lease reserves its whole credit in the shared guest ledger.  Do not
		// lease the final partial window: frame/control admission must still be
		// able to consume the remaining budget, and the owner reservation path
		// is exact for these small heaps.
		if credit >= managedBatchCredit {
			batch, _ = heap.acquireBatch(credit, managedBatchHandles)
		}
		// A lease is an optional fast path. Tiny heaps may be unable to admit
		// the nursery pair even though the owner allocator can still produce the
		// precise resource-limit diagnostic for this frame.
		if batch != nil {
			task.allocationBatch = batch
			defer func() {
				if err != nil || task.retryInstruction {
					batch.abortUnfinished()
				}
				if flushErr := batch.flush(); err == nil && flushErr != nil {
					err = flushErr
				}
				task.retainedFrames(func(_ int, frame *executionFrame) bool {
					if frame != nil {
						frame.frame.allocationBatch = nil
					}
					return true
				})
				task.allocationBatch = nil
			}()
		}
	}
	for {
		task.preparingSelection = nil
		if machine.vm.heap != nil && machine.vm.heap.collectionRequested.Load() {
			return taskYield{kind: taskYieldCooperate}, nil, nil
		}
		if machine.vm.controlWaiters.Load() != 0 {
			return taskYield{kind: taskYieldPoll}, nil, nil
		}
		if task.pendingErr != nil {
			err := task.pendingErr
			task.pendingErr = nil
			return taskYield{}, nil, err
		}
		if len(task.frames) == 0 {
			return taskYield{kind: taskYieldComplete}, nil, errors.New("execution task has no frame")
		}
		if limit := machine.vm.limits.MaxCallDepth; limit > 0 && len(task.frames) > limit {
			return taskYield{}, nil, fmt.Errorf("execution call depth limit exceeded: max %d", limit)
		}
		current := task.frames[len(task.frames)-1]
		if current != nil && current.frame != nil {
			current.frame.allocationBatch = task.allocationBatch
		}
		if current.completionActive {
			machine.controlMu.Lock()
			values, done, err := machine.advanceCompletion(task)
			machine.controlMu.Unlock()
			if err != nil || done {
				return taskYield{kind: taskYieldComplete}, values, err
			}
			continue
		}
		callFrame := current.frame
		functionID := callFrame.function.Decl.ID
		if callFrame.pc >= len(callFrame.function.Instructions) {
			var values []vmValue
			var err error
			if callFrame.hasResultLocals() {
				values, err = callFrame.currentResultValues()
			} else {
				values, err = callFrame.normalizeReturnValues(nil)
			}
			if err != nil {
				return taskYield{}, nil, Error{
					Generation:  callFrame.revisionGeneration(),
					ProgramHash: callFrame.revisionHash(),
					ModulePath:  callFrame.module.modulePath(),
					FunctionID:  functionID,
					Err:         err,
				}
			}
			current.setCompletion(frameCompletion{returnValues: values})
			continue
		}
		pc := callFrame.pc
		inst := &callFrame.function.Instructions[pc]
		retrying := task.retryInstruction
		if task.execution != nil && task.execution.cancelRequested.Load() {
			return taskYield{}, nil, context.Canceled
		}
		if machine.vm.interruptRequested.Load() {
			return taskYield{}, nil, context.Canceled
		}
		if !retrying && task.quantumSteps >= taskInstructionQuantum {
			return taskYield{kind: taskYieldCooperate}, nil, nil
		}
		if !retrying && slice.limit > 0 && slice.attempted >= slice.limit {
			return taskYield{kind: taskYieldPoll}, nil, nil
		}
		if retrying {
			callFrame.releasePopValues()
			task.retryInstruction = false
		} else {
			slice.attempted++
			if task.quantumSteps < taskInstructionQuantum {
				task.quantumSteps++
			}
		}
		debugging := !retrying && (machine.vm.breakpointsActive.Load() || machine.vm.debugStepActive.Load() || machine.vm.hostPauseRequested.Load())
		if debugging {
			machine.controlMu.Lock()
			if _, ok := machine.vm.checkDebugPause(task, callFrame, functionID, pc); ok {
				if machine.vm.paused == nil {
					machine.vm.paused = &debugPauseState{frame: callFrame, functionID: functionID}
				}
				machine.controlMu.Unlock()
				return taskYield{kind: taskYieldPause}, nil, nil
			}
			machine.controlMu.Unlock()
		}
		if !retrying {
			if err := task.consumeStep(machine.vm.maxSteps); err != nil {
				if errors.Is(err, errStepBudgetReserved) {
					return taskYield{kind: taskYieldBudget}, nil, nil
				}
				return taskYield{}, nil, Error{
					Generation:         callFrame.revisionGeneration(),
					ProgramHash:        callFrame.revisionHash(),
					ModulePath:         callFrame.module.modulePath(),
					FunctionID:         functionID,
					PC:                 pc,
					Op:                 inst.opcodeText(),
					ExecutionContextID: callFrame.executionContextID,
					Loc:                runtimeLocation(callFrame, functionID, pc),
					Err:                err,
				}
			}
		}
		callFrame.beginSlotInstruction(pc, inst)
		callFrame.pc++
		if !retrying {
			slice.executed++
		}
		if execution := task.execution; !retrying && execution != nil && execution.profileEvery != 0 && task.profilePhase&execution.profileMask == 0 {
			execution.recordGuestSample(callFrame, functionID, pc, inst)
		}
		if inst.control {
			yield, handled, err := machine.runControlInstruction(task, current, pc, inst)
			if err != nil {
				return taskYield{}, nil, err
			}
			if !handled {
				return taskYield{}, nil, machine.vm.runtimeInstructionError(callFrame, functionID, pc, inst, errors.New("prepared control opcode is not handled"))
			}
			if yield.kind != "" {
				return yield, nil, nil
			}
			continue
		}
		yield, err := machine.runDataInstruction(task, current, pc, inst)
		if err != nil {
			return taskYield{}, nil, err
		}
		if yield.kind != "" {
			return yield, nil, nil
		}
	}
}
