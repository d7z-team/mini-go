package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	current := machine.vm.revision.Load()
	if current != nil && frame.revision == current &&
		inst.callModuleIndex >= 0 && inst.callModuleIndex < len(current.moduleOrder) {
		module := current.moduleOrder[inst.callModuleIndex]
		if module != nil && inst.callFunctionIndex >= 0 && inst.callFunctionIndex < len(module.executable.FunctionOrder) {
			return module, module.executable.FunctionOrder[inst.callFunctionIndex], nil
		}
	}
	modulePath := inst.callModule
	if modulePath == "" {
		modulePath = strings.TrimSpace(inst.call.ModulePath)
		if modulePath == "" {
			modulePath = frame.module.modulePath()
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
	initFrame, err := machine.vm.newExecutionFrame(module, moduleInitFunctionID, nil, nil, task.id, 0, false)
	if err != nil {
		return err
	}
	// Frame preparation may request a guest census. Do not publish the
	// initializing state until preparation succeeds, otherwise retrying the
	// instruction observes its own transient request as a permanent module
	// failure and can loop without executing another bytecode step.
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
	taskYieldCensus    = "census"
)

func (vm *vm) newExecutionFrame(module *moduleInstance, functionID string, args []vmValue, upvalues map[string]*slot, contextID int64, expectedResults int, qualifyResults bool) (*executionFrame, error) {
	if module == nil || module.executable == nil {
		return nil, errors.New("nil module")
	}
	fn, ok := module.executable.Functions[functionID]
	if !ok {
		return nil, fmt.Errorf("unknown function %q", functionID)
	}
	return vm.newPreparedExecutionFrame(module, fn, args, upvalues, contextID, expectedResults, qualifyResults)
}

func (vm *vm) newPreparedExecutionFrame(module *moduleInstance, fn loadedFunction, args []vmValue, upvalues map[string]*slot, contextID int64, expectedResults int, qualifyResults bool) (*executionFrame, error) {
	if len(args) != len(fn.Decl.Signature.Params) {
		return nil, fmt.Errorf("function %s argument count mismatch: got %d, want %d", fn.Decl.ID, len(args), len(fn.Decl.Signature.Params))
	}
	if expectedResults >= 0 && expectedResults != len(fn.Decl.Signature.Results) {
		return nil, fmt.Errorf("function %s result count mismatch: got %d, want %d", fn.Decl.ID, expectedResults, len(fn.Decl.Signature.Results))
	}
	resultSlots := expectedResults
	if resultSlots < 0 {
		resultSlots = 0
	}
	callFrame, allocated, err := newFrame(module, fn, args, upvalues, contextID)
	if err != nil {
		return nil, err
	}
	if allocated {
		if err := vm.chargeRuntimeObject(len(fn.Decl.Locals)+fn.MaxStack+len(upvalues)+resultSlots, 0); err != nil {
			callFrame.recycle()
			return nil, err
		}
	}
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
	root, err := vm.newExecutionFrame(module, functionID, args, nil, contextID, -1, false)
	if err != nil {
		return Error{
			Generation:  module.revision.generation,
			ProgramHash: module.revision.code.image.Hash,
			ModulePath:  module.modulePath(),
			FunctionID:  functionID,
			Err:         err,
		}
	}
	if err := vm.chargeAllocation(); err != nil {
		root.frame.recycle()
		return err
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
			// Poll and census yields deliberately return to the caller. A census can
			// execute no bytecode, so retrying it inside this loop could otherwise
			// turn one bounded PollSteps call into an unbounded control loop.
			if yield.kind == taskYieldPoll || yield.kind == taskYieldCensus {
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
		if len(machine.blocked) != 0 {
			// A task between queues still owns a continuation. Its running or
			// parking lease is a progress source, not evidence of deadlock.
			if len(machine.tasks) != len(machine.blocked) {
				return runOutcome{state: ExecutionPending}
			}
			if len(machine.vm.timers) != 0 {
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
}

func (machine *executionMachine) runTask(task *executionTask, slice *taskSlice) (taskYield, []vmValue, error) {
	defer task.releaseStepGrant()
	defer func() { task.preparingSelection = nil }()
	if task.blocked != nil {
		if task.blocked.ticket == nil {
			task.blocked.ticket = task.ownership.beginWait()
		}
		return taskYield{kind: taskYieldBlocked}, nil, nil
	}
	for {
		task.preparingSelection = nil
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
		if current.completion != nil {
			machine.controlMu.Lock()
			values, done, err := machine.advanceCompletion(task)
			machine.controlMu.Unlock()
			if census, ok := findGuestCensusRequest(err); ok {
				if len(task.frames) == 0 || task.frames[len(task.frames)-1].frame.pc == 0 {
					return taskYield{}, nil, errors.New("census continuation has no caller instruction")
				}
				caller := task.frames[len(task.frames)-1].frame
				caller.pc--
				task.retryInstruction = true
				task.pendingCensus = census.bytes
				return taskYield{kind: taskYieldCensus}, nil, nil
			}
			if err != nil || done {
				return taskYield{kind: taskYieldComplete}, values, err
			}
			continue
		}
		callFrame := current.frame
		functionID := callFrame.function.Decl.ID
		if callFrame.pc >= len(callFrame.function.Instructions) {
			if len(callFrame.stack) != 0 {
				return taskYield{}, nil, Error{
					Generation:  callFrame.revisionGeneration(),
					ProgramHash: callFrame.revisionHash(),
					ModulePath:  callFrame.module.modulePath(),
					FunctionID:  functionID,
					Err:         fmt.Errorf("function completed with %d stack values", len(callFrame.stack)),
				}
			}
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
			current.completion = &frameCompletion{returnValues: values}
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
			callFrame.stack = append(callFrame.stack, callFrame.popValues...)
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
