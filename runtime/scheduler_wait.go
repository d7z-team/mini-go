package runtime

import (
	"errors"
	"fmt"
	"sort"
)

const maxBlockedContexts = 64

func (machine *executionMachine) blockTask(task *executionTask, current *executionFrame, pc int, inst *preparedInstruction, operation *blockedOperation) {
	blocked := WaitBlockedError{Message: "execution blocked"}
	switch operation.kind {
	case "mutex":
		blocked.Message = "mutex lock is pending"
	case "select":
		blocked.Message = "channel selection is pending"
	case "ffi":
		blocked.Message = "FFI call is pending"
	case "module":
		blocked.Message = "module initialization is pending"
	}
	operation.error = machine.vm.runtimeInstructionError(current.frame, current.frame.function.Decl.ID, pc, inst, blocked)
	if operation.ticket == nil {
		operation.ticket = task.ownership.beginWait()
	}
	task.blocked = operation
	if operation.selection != nil {
		operation.selection.owner, operation.selection.ticket = &task.ownership, operation.ticket
		if operation.selection.done {
			task.ownership.notify(operation.ticket)
		}
	}
	if operation.ffi != nil {
		operation.ffi.bindWait(&task.ownership, operation.ticket)
	}
}

func (machine *executionMachine) allBlockedError() Error {
	total, contexts := machine.blockedContextSnapshot()
	blocked := AllBlockedError{Total: total, Contexts: contexts}
	out := Error{Err: blocked}
	if len(contexts) != 0 {
		first := contexts[0]
		out.Generation, out.ProgramHash = first.Revision.Generation, first.Revision.Hash
		out.ModulePath, out.FunctionID = first.ModulePath, first.FunctionID
		out.PC, out.Op, out.ExecutionContextID, out.Loc = first.PC, first.Op, first.ExecutionContextID, first.Loc
	}
	return out
}

func (machine *executionMachine) blockedContextSnapshot() (int, []BlockedContext) {
	contexts := make([]BlockedContext, 0, min(len(machine.blocked), maxBlockedContexts))
	total := 0
	for _, task := range machine.blocked {
		if task == nil || task.blocked == nil {
			continue
		}
		total++
		runtimeErr := task.blocked.error
		reason := "execution blocked"
		var blocked WaitBlockedError
		if errors.As(runtimeErr.Err, &blocked) && blocked.Message != "" {
			reason = blocked.Message
		}
		context := BlockedContext{
			ExecutionContextID: runtimeErr.ExecutionContextID,
			Revision:           RevisionInfo{Generation: runtimeErr.Generation, Hash: runtimeErr.ProgramHash},
			ModulePath:         runtimeErr.ModulePath, FunctionID: runtimeErr.FunctionID,
			PC: runtimeErr.PC, Op: runtimeErr.Op, Loc: runtimeErr.Loc, Reason: reason,
		}
		if task.scope != nil {
			context.ScopeID = task.scope.id
		}
		index := sort.Search(len(contexts), func(i int) bool {
			if contexts[i].ExecutionContextID != context.ExecutionContextID {
				return contexts[i].ExecutionContextID > context.ExecutionContextID
			}
			return contexts[i].ScopeID > context.ScopeID
		})
		if index == maxBlockedContexts {
			continue
		}
		if len(contexts) < maxBlockedContexts {
			contexts = append(contexts, BlockedContext{})
		}
		copy(contexts[index+1:], contexts[index:len(contexts)-1])
		contexts[index] = context
	}
	return total, contexts
}

func (machine *executionMachine) wakeBlocked() error {
	if err := machine.vm.drainReadyTimers(); err != nil {
		return err
	}
	for {
		progress := false
		for i := 0; i < len(machine.blocked); {
			task := machine.blocked[i]
			op := task.blocked
			ready, err := machine.resumeBlocked(task, op)
			if err != nil {
				return err
			}
			if !ready {
				i++
				continue
			}
			if !task.ownership.resume(op.ticket) {
				return errors.New("blocked task resumed before ownership handoff")
			}
			task.blocked = nil
			copy(machine.blocked[i:], machine.blocked[i+1:])
			machine.blocked[len(machine.blocked)-1] = nil
			machine.blocked = machine.blocked[:len(machine.blocked)-1]
			machine.pushRunnable(task)
			progress = true
		}
		if !progress {
			return nil
		}
	}
}

func (machine *executionMachine) resumeBlocked(task *executionTask, op *blockedOperation) (bool, error) {
	if op == nil || len(task.frames) == 0 {
		return false, errors.New("blocked task lost continuation")
	}
	callFrame := task.frames[len(task.frames)-1].frame
	switch op.kind {
	case "select":
		if !op.selection.tryCommit() {
			return false, nil
		}
		if op.reflectSelect != nil {
			values, err := op.reflectSelect.complete()
			if err != nil {
				return false, err
			}
			for _, value := range values {
				callFrame.push(value)
			}
			return true, nil
		}
		if op.reflectSend && op.selection.err == nil {
			callFrame.push(newVMValue("String", ""))
			callFrame.push(newVMValue("Bool", true))
			return true, nil
		}
		if op.reflectRecv != nil && op.selection.err == nil {
			values, err := op.reflectRecv.complete(op.selection.value, op.selection.ok)
			if err != nil {
				return false, err
			}
			for _, value := range values {
				callFrame.push(value)
			}
			return true, nil
		}
		if err := op.selection.deliver(callFrame, op.selectPayload, op.withOK); err != nil {
			var fault *guestPanic
			if !errors.As(err, &fault) {
				return false, err
			}
			current := task.frames[len(task.frames)-1]
			machine.startPanic(task, current, current.frame.pc-1, fault.value)
		}
		return true, nil
	case "mutex":
		resource := op.mutex.resource
		if resource.grant != op.mutex.waiter {
			return false, nil
		}
		resource.grant = nil
		resource.locked = true
		return true, nil
	case "module":
		if op.module.state.initState == moduleInitializing {
			return false, nil
		}
		if op.module.state.initState == moduleFailed {
			task.pendingErr = op.module.state.initErr
		} else if op.moduleReady != nil {
			task.pendingErr = op.moduleReady(task, task.frames[len(task.frames)-1])
		}
		return true, nil
	case "ffi":
		if !task.ownership.notified(op.ticket) {
			return false, nil
		}
		values, ready := op.ffi.take()
		if !ready {
			return false, nil
		}
		for _, value := range values {
			callFrame.push(value)
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown blocked operation %q", op.kind)
	}
}
