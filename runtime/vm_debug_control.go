package runtime

import (
	"context"
	"errors"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func (vm *vm) requestPause() error {
	if vm == nil {
		return errors.New("nil VM")
	}
	vm.hostPauseRequested.Store(true)
	return nil
}

func (vm *vm) resumePaused(stepMode debugStepMode) runOutcome {
	if vm == nil {
		return failedRun(errors.New("nil VM"))
	}
	if vm.paused == nil {
		return failedRun(errors.New("vm is not paused"))
	}
	if vm.machine == nil || vm.machine.paused == nil {
		return failedRun(errors.New("paused VM has no execution task"))
	}
	task := vm.machine.paused
	state := vm.paused
	vm.paused = nil
	vm.debugger.clearPausedEvent()
	vm.skipDebugPause = debugResumePoint{
		TaskID:     task.id,
		Generation: state.frame.revisionGeneration(),
		ModulePath: state.frame.module.executable.Artifact.Module.Path,
		FunctionID: state.functionID,
		PC:         state.frame.pc,
		Active:     true,
	}
	if stepMode != "" {
		vm.debugStep = debugStepState{
			TaskID:     task.id,
			Mode:       stepMode,
			StartDepth: len(task.debugParents) + len(task.frames),
			Active:     true,
		}
		vm.debugStepActive.Store(true)
	}
	return vm.runPrepared(defaultPollQuantum)
}

func (vm *vm) shouldPauseForStep(task *executionTask) bool {
	if vm == nil || task == nil || !vm.debugStep.Active || vm.debugStep.TaskID != task.id {
		return false
	}
	switch vm.debugStep.Mode {
	case debugStepInto:
		return true
	case debugStepOver:
		return len(task.debugParents)+len(task.frames) <= vm.debugStep.StartDepth
	case debugStepOut:
		return len(task.debugParents)+len(task.frames) < vm.debugStep.StartDepth
	default:
		return false
	}
}

func (vm *vm) beginRun() int64 {
	if vm == nil {
		return 0
	}
	vm.nextRunID++
	return vm.nextRunID
}

func (vm *vm) finishForeground() {
	if vm == nil {
		return
	}
	vm.paused = nil
	vm.debugger.clearPausedEvent()
	vm.skipDebugPause = debugResumePoint{}
	vm.debugStep = debugStepState{}
	vm.debugStepActive.Store(false)
	vm.hostPauseRequested.Store(false)
	if vm.machine != nil {
		vm.machine.foreground = nil
		if len(vm.machine.scopes) == 0 {
			vm.machine = nil
		}
	}
}

func (vm *vm) finishRun() {
	if vm == nil {
		return
	}
	if vm.machine != nil {
		vm.machine.cancelAllScopes(context.Canceled)
	}
	vm.closeTimers()
	vm.closeFFICalls()
	vm.paused = nil
	vm.debugger.clearPausedEvent()
	vm.skipDebugPause = debugResumePoint{}
	vm.debugStep = debugStepState{}
	vm.debugStepActive.Store(false)
	vm.hostPauseRequested.Store(false)
	vm.machine = nil
	if vm.instance != nil {
		vm.instance.discardBackgroundPause()
	}
}

func (vm *vm) nextSpawnExecutionContextID() int64 {
	if vm == nil {
		return 0
	}
	vm.nextExecutionContextID++
	return vm.nextExecutionContextID
}

func runtimeLocation(frame *frame, functionID string, pc int) *ir.Location {
	if frame == nil || frame.revision == nil || frame.module == nil {
		return nil
	}
	loc, ok := frame.revision.symbols.nearestLocation(frame.module.modulePath(), functionID, pc)
	if !ok {
		return nil
	}
	return &loc
}

func (vm *vm) runtimeInstructionError(frame *frame, functionID string, pc int, inst *preparedInstruction, err error) Error {
	return Error{
		Generation:         frame.revisionGeneration(),
		ProgramHash:        frame.revisionHash(),
		ModulePath:         frame.module.executable.Artifact.Module.Path,
		FunctionID:         functionID,
		PC:                 pc,
		Op:                 inst.opcodeText(),
		ExecutionContextID: frame.executionContextID,
		Loc:                runtimeLocation(frame, functionID, pc),
		Err:                err,
	}
}
