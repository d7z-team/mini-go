package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type guestPanic struct {
	value vmValue
	err   error
}

func (p *guestPanic) Error() string {
	if p == nil || p.err == nil {
		return "panic"
	}
	return p.err.Error()
}

func (p *guestPanic) Unwrap() error {
	if p == nil {
		return nil
	}
	return p.err
}

func newGuestPanic(err error) error {
	if err == nil {
		return nil
	}
	return &guestPanic{value: newVMValue("String", err.Error()), err: err}
}

func (machine *executionMachine) startPanic(task *executionTask, current *executionFrame, pc int, value vmValue) {
	value = normalizePanicValue(value)
	panicState := &machinePanic{
		err: panicError{
			Value:       value,
			Generation:  current.frame.revisionGeneration(),
			ProgramHash: current.frame.revisionHash(),
			ModulePath:  current.frame.module.executable.Artifact.Module.Path,
			FunctionID:  current.frame.function.Decl.ID,
			PC:          pc,
			Loc:         runtimeLocation(current.frame, current.frame.function.Decl.ID, pc),
		},
	}
	if event, ok := machine.vm.debugPanicEvent(task, current.frame, current.frame.function.Decl.ID, pc, value); ok {
		panicState.debugEvent = &event
	}
	panicState.runtimeErr = Error{
		Generation:         current.frame.revisionGeneration(),
		ProgramHash:        current.frame.revisionHash(),
		ModulePath:         panicState.err.ModulePath,
		FunctionID:         panicState.err.FunctionID,
		PC:                 panicState.err.PC,
		Op:                 string(ir.OpPanic),
		ExecutionContextID: task.id,
		Loc:                panicState.err.Loc,
		Err:                panicState.err,
		stack:              machine.panicStack(task, current, pc),
	}
	current.completion = &frameCompletion{panic: panicState}
}

func (machine *executionMachine) panicStack(task *executionTask, current *executionFrame, pc int) []runtimeStackFrame {
	stack := make([]runtimeStackFrame, 0, len(task.debugParents)+len(task.frames))
	for index := len(task.frames) - 1; index >= 0; index-- {
		callFrame := task.frames[index].frame
		framePC := callFrame.pc - 1
		if callFrame == current.frame {
			framePC = pc
		}
		if framePC < 0 {
			framePC = 0
		}
		stack = append(stack, runtimeStackFrame{
			Revision:           RevisionInfo{Generation: callFrame.revisionGeneration(), Hash: callFrame.revisionHash()},
			ExecutionContextID: task.id,
			ModulePath:         callFrame.module.modulePath(),
			FunctionID:         callFrame.function.Decl.ID,
			PC:                 framePC,
			Loc:                runtimeLocation(callFrame, callFrame.function.Decl.ID, framePC),
		})
	}
	for index := len(task.debugParents) - 1; index >= 0; index-- {
		parent := task.debugParents[index]
		loc := parent.Loc
		stack = append(stack, runtimeStackFrame{
			Revision:           RevisionInfo{Generation: parent.Generation, Hash: parent.ProgramHash},
			ExecutionContextID: parent.ExecutionContextID,
			ModulePath:         parent.ModulePath,
			FunctionID:         parent.FunctionID,
			PC:                 parent.PC,
			Loc:                &loc,
		})
	}
	return stack
}

func (machine *executionMachine) advanceCompletion(task *executionTask) ([]vmValue, bool, error) {
	defer task.releaseCompletingFrame()
	for {
		if len(task.frames) == 0 {
			return nil, false, errors.New("completion has no frame")
		}
		current := task.frames[len(task.frames)-1]
		if len(current.frame.defers) != 0 {
			last := len(current.frame.defers) - 1
			call := current.frame.defers[last]
			current.frame.defers[last] = deferredCall{}
			current.frame.defers = current.frame.defers[:last]
			module, err := machine.vm.moduleForFunctionRef(call.caller, call.ref)
			if err != nil {
				return nil, false, err
			}
			deferred, err := machine.vm.newExecutionFrame(module, call.ref.FunctionID, nil, call.ref.upvalues, task.id, 0, false)
			if err != nil {
				return nil, false, err
			}
			deferred.deferred = true
			task.frames = append(task.frames, deferred)
			return nil, false, nil
		}
		if current.completion.resume {
			current.completion = nil
			return nil, false, nil
		}
		if current.completion.panic == nil && current.frame.hasResultLocals() {
			values, err := current.frame.currentResultValues()
			if err != nil {
				return nil, false, err
			}
			current.completion.returnValues = values
		}
		lastFrame := len(task.frames) - 1
		task.frames[lastFrame] = nil
		task.frames = task.frames[:lastFrame]
		task.completingFrame = current
		if current.deferred {
			if len(task.frames) == 0 {
				return nil, false, errors.New("deferred call has no owner frame")
			}
			owner := task.frames[len(task.frames)-1]
			if current.completion.panic != nil {
				owner.completion = &frameCompletion{panic: current.completion.panic}
			} else {
				if len(current.completion.returnValues) != 0 {
					return nil, false, fmt.Errorf("deferred function returned %d values", len(current.completion.returnValues))
				}
				if current.recoveredPanic != nil && current.recoveredPanic == owner.completion.panic {
					values, err := owner.frame.currentResultValues()
					if err != nil {
						return nil, false, err
					}
					owner.completion = &frameCompletion{returnValues: values}
				}
			}
			task.releaseCompletingFrame()
			continue
		}
		if current.completion.panic != nil {
			if current.abort != nil {
				current.abort()
				current.abort = nil
			}
			if len(task.frames) != 0 {
				owner := task.frames[len(task.frames)-1]
				owner.completion = &frameCompletion{panic: current.completion.panic}
				task.releaseCompletingFrame()
				continue
			}
			panicState := current.completion.panic
			if panicState.debugEvent != nil {
				machine.vm.appendDebugEvent(*panicState.debugEvent)
			}
			return nil, false, panicState.runtimeErr
		}
		values := current.completion.returnValues
		if current.qualifyResults {
			values = current.frame.module.qualifyValuesForExport(values)
		}
		if len(task.frames) == 0 {
			// Explicit return values may still occupy the completed frame's
			// stack backing. The result also crosses the VM ownership boundary,
			// so value composites must not expose module state to the caller.
			values = append([]vmValue(nil), values...)
			for i := range values {
				values[i] = current.frame.module.cloneValueForResult(values[i])
			}
			if current.resume != nil {
				if err := current.resume(task, nil, values); err != nil {
					return nil, false, err
				}
			}
			return values, true, nil
		}
		if len(values) != current.expectedResults {
			return nil, false, fmt.Errorf("call %s returned %d values, expected %d", current.frame.function.Decl.ID, len(values), current.expectedResults)
		}
		caller := task.frames[len(task.frames)-1]
		if current.resume != nil {
			err := current.resume(task, caller, values)
			return nil, false, err
		}
		for _, value := range values {
			caller.frame.push(value)
		}
		return nil, false, nil
	}
}

func (task *executionTask) releaseCompletingFrame() {
	current := task.completingFrame
	if current == nil {
		return
	}
	if current.abort != nil {
		current.abort()
		current.abort = nil
	}
	task.completingFrame = nil
	current.frame.recycle()
}

func (machine *executionMachine) abortTask(task *executionTask) {
	if task == nil {
		return
	}
	machine.cancelBlockedOperation(task)
	task.releaseStepGrant()
	task.releaseCompletingFrame()
	for _, active := range task.frames {
		if active.abort != nil {
			active.abort()
			active.abort = nil
		}
		active.frame.recycle()
	}
	task.frames = nil
}

func (machine *executionMachine) cancelBlockedOperation(task *executionTask) {
	if task == nil || task.blocked == nil {
		return
	}
	operation := task.blocked
	task.blocked = nil
	switch operation.kind {
	case "select":
		operation.selection.unregister()
		operation.selection.owner, operation.selection.ticket = nil, nil
	case "mutex":
		operation.mutex.resource.cancel(operation.mutex.waiter)
	case "ffi":
		if operation.ffi != nil {
			operation.ffi.stop()
		}
	}
}

func (machine *executionMachine) abortTasks(err error) {
	for _, task := range machine.tasks {
		machine.abortTask(task)
		machine.finishTask(task, err)
	}
	machine.runnable = nil
	machine.runnableHead = 0
	machine.blocked = nil
	machine.paused = nil
}

func (machine *executionMachine) recoverValue(task *executionTask) vmValue {
	if len(task.frames) < 2 {
		return newVMValue("Any", nil)
	}
	top := len(task.frames) - 1
	if !task.frames[top].deferred {
		top--
	}
	if top < 1 || !task.frames[top].deferred || task.frames[top].recoveredPanic != nil {
		return newVMValue("Any", nil)
	}
	owner := task.frames[top-1].completion
	if owner == nil || owner.panic == nil {
		return newVMValue("Any", nil)
	}
	task.frames[top].recoveredPanic = owner.panic
	return newVMValue("Any", owner.panic.err.Value)
}

const panicNilMessage = "panic called with nil argument"

func normalizePanicValue(value vmValue) vmValue {
	if value.Data != nil {
		return value
	}
	if value.Type.Ref.Kind == types.Any {
		return newVMValue("String", panicNilMessage)
	}
	if _, ok := value.Type.InterfaceInfo(); ok {
		return newVMValue("String", panicNilMessage)
	}
	return value
}
