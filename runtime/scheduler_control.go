package runtime

import (
	"errors"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func (machine *executionMachine) executeControl(task *executionTask, current *executionFrame, pc int, inst *preparedInstruction) (taskYield, bool, error) {
	callFrame := current.frame
	yield, handled, err := machine.executeControlBody(task, current, pc, inst)
	if _, census := findGuestCensusRequest(err); !census {
		callFrame.releasePopValues()
	}
	task.preparingSelection = nil
	return yield, handled, err
}

func (machine *executionMachine) executeControlBody(task *executionTask, current *executionFrame, pc int, inst *preparedInstruction) (taskYield, bool, error) {
	callFrame := current.frame
	switch inst.op {
	case preparedSelect:
		payload := inst.selection
		cases := make([]channelSelectCase, len(payload.Cases))
		for index, selected := range payload.Cases {
			cases[index].channel = callFrame.localCells[callFrame.function.LocalIndexes[selected.Channel]].load()
			if selected.Send != "" {
				cases[index].send = true
				cases[index].value = callFrame.localCells[callFrame.function.LocalIndexes[selected.Send]].load()
			}
		}
		selection, err := machine.vm.prepareChannelSelection(task, callFrame.module, cases)
		if err != nil {
			return taskYield{}, true, err
		}
		if !selection.tryCommit() {
			if payload.Default {
				selection.complete(-1, vmValue{}, false)
			} else {
				selection.register()
				machine.blockTask(task, current, pc, inst, &blockedOperation{kind: "select", selection: selection, selectPayload: payload})
				return taskYield{kind: taskYieldBlocked}, true, nil
			}
		}
		return taskYield{}, true, selection.deliver(callFrame, payload, false)
	case preparedAddressOf:
		payload := inst.address
		module, err := machine.module(payload.ModulePath)
		if err != nil {
			return taskYield{}, true, err
		}
		ready := func(_ *executionTask, caller *executionFrame) error {
			export, ok := module.executable.Exports[payload.Export]
			if !ok || export.Kind != "global" {
				return fmt.Errorf("address target %s.%s is not an exported variable", payload.ModulePath, payload.Export)
			}
			if err := machine.vm.chargeAllocation(); err != nil {
				return err
			}
			value, err := caller.frame.slotAddress(module.state.globals[export.ID], *payload)
			if err != nil {
				return err
			}
			caller.frame.push(value)
			return nil
		}
		if module.state.initState == moduleInitializing && module.state.initTask == task {
			return taskYield{}, true, ready(task, current)
		}
		return taskYield{}, true, machine.initializeModule(task, current, module, ready)
	case preparedLoadExport:
		payload := inst.export
		module, err := machine.module(payload.ModulePath)
		if err != nil {
			return taskYield{}, true, err
		}
		ready := func(_ *executionTask, caller *executionFrame) error {
			value, err := loadInitializedExport(module, payload.Export)
			if err != nil {
				return err
			}
			caller.frame.push(value)
			return nil
		}
		if module.state.initState == moduleInitializing && module.state.initTask == task {
			return taskYield{}, true, ready(task, current)
		}
		return taskYield{}, true, machine.initializeModule(task, current, module, ready)
	case preparedInitModule:
		payload := inst.initModule
		module, err := machine.module(payload.ModulePath)
		if err != nil {
			return taskYield{}, true, err
		}
		return taskYield{}, true, machine.initializeModule(task, current, module, nil)
	case preparedCallDirect:
		payload := inst.call
		target, function, err := machine.directCallTarget(callFrame, inst)
		if err != nil {
			return taskYield{}, true, err
		}
		if target.state != callFrame.module.state && target.state.initState != moduleReady && target.state.initTask != task {
			return taskYield{}, true, machine.initializeModule(task, current, target, func(task *executionTask, caller *executionFrame) error {
				args, err := caller.frame.popN(payload.ArgCount)
				if err != nil {
					return err
				}
				qualify := target.modulePath() != caller.frame.module.modulePath()
				if qualify {
					args = caller.frame.module.qualifyValuesForArgumentBoundary(args)
				}
				callee, err := machine.vm.newPreparedExecutionFrame(target, function, args, nil, task.id, payload.ResultCount, qualify)
				if _, census := findGuestCensusRequest(err); !census {
					caller.frame.releasePopValues()
				}
				if err != nil {
					return err
				}
				task.frames = append(task.frames, callee)
				return nil
			})
		}
		args, err := callFrame.popN(payload.ArgCount)
		if err != nil {
			return taskYield{}, true, err
		}
		qualify := target.modulePath() != callFrame.module.modulePath()
		if qualify {
			args = callFrame.module.qualifyValuesForArgumentBoundary(args)
		}
		callee, err := machine.vm.newPreparedExecutionFrame(target, function, args, nil, task.id, payload.ResultCount, qualify)
		if err != nil {
			return taskYield{}, true, err
		}
		task.frames = append(task.frames, callee)
		return taskYield{}, true, nil
	case preparedTailCallDirect:
		payload := inst.call
		target, function, err := machine.directCallTarget(callFrame, inst)
		if err != nil {
			return taskYield{}, true, err
		}
		if target.state != callFrame.module.state && target.state.initState != moduleReady && target.state.initTask != task {
			err = machine.initializeModule(task, current, target, func(task *executionTask, caller *executionFrame) error {
				return machine.enterDirectTailCall(task, caller, target, function, payload)
			})
			return taskYield{}, true, err
		}
		return taskYield{}, true, machine.enterDirectTailCall(task, current, target, function, payload)
	case preparedCallValue:
		payload := inst.call
		values, err := callFrame.popN(payload.ArgCount + 1)
		if err != nil {
			return taskYield{}, true, err
		}
		if target, ok := values[0].Data.(reflectMakeFuncTarget); ok {
			args := values[1:]
			if target.handlerModule != callFrame.module {
				args = callFrame.module.qualifyValuesForArgumentBoundary(args)
			}
			request, err := target.callbackRequest(machine.vm, args)
			if err != nil {
				return taskYield{}, true, err
			}
			callee, err := machine.vm.newExecutionFrame(request.module, request.functionID, request.args, request.upvalues, task.id, request.resultCount, false)
			if err != nil {
				return taskYield{}, true, err
			}
			callee.resume = machine.reflectCallCompletion(request.result, payload.ResultCount, "reflect MakeFunc returned")
			task.frames = append(task.frames, callee)
			return taskYield{}, true, nil
		}
		target, ref, err := machine.vm.resolveFunctionValue(callFrame.module, values[0])
		if err != nil {
			if errors.Is(err, errNilFunctionCall) {
				machine.startPanic(task, current, pc, newVMValue("String", errNilFunctionCall.Error()))
				return taskYield{}, true, nil
			}
			return taskYield{}, true, err
		}
		args := values[1:]
		qualify := target.modulePath() != callFrame.module.modulePath()
		if qualify {
			args = callFrame.module.qualifyValuesForArgumentBoundary(args)
		}
		callee, err := machine.vm.newExecutionFrame(target, ref.FunctionID, args, ref.upvalues, task.id, payload.ResultCount, qualify)
		if err != nil {
			return taskYield{}, true, err
		}
		task.frames = append(task.frames, callee)
		return taskYield{}, true, nil
	case preparedCallInterface:
		payload := inst.interfaceCall
		values, err := callFrame.popN(payload.ArgCount + 1)
		if err != nil {
			return taskYield{}, true, err
		}
		receiver, target, functionID, err := callFrame.module.resolveInterfaceMethod(values[0], callFrame.module.formatType(payload.InterfaceType), payload.Method)
		if err != nil {
			return taskYield{}, true, err
		}
		target, err = machine.module(target.modulePath())
		if err != nil {
			return taskYield{}, true, err
		}
		args := append([]vmValue{receiver}, values[1:]...)
		qualify := target.modulePath() != callFrame.module.modulePath()
		if qualify {
			args = callFrame.module.qualifyValuesForArgumentBoundary(args)
		}
		callee, err := machine.vm.newExecutionFrame(target, functionID, args, nil, task.id, payload.ResultCount, qualify)
		if err != nil {
			return taskYield{}, true, err
		}
		task.frames = append(task.frames, callee)
		return taskYield{}, true, nil
	case preparedSpawn:
		payload := inst.call
		values, err := callFrame.popN(payload.ArgCount + 1)
		if err != nil {
			return taskYield{}, true, err
		}
		target, ref, err := machine.vm.resolveFunctionValue(callFrame.module, values[0])
		if err != nil {
			return taskYield{}, true, err
		}
		args := values[1:]
		if target.modulePath() != callFrame.module.modulePath() {
			args = callFrame.module.qualifyValuesForArgumentBoundary(args)
		}
		childID := machine.vm.nextSpawnExecutionContextID()
		childFrame, err := machine.vm.newExecutionFrame(target, ref.FunctionID, args, ref.upvalues, childID, payload.ResultCount, false)
		if err != nil {
			return taskYield{}, true, err
		}
		if err := machine.vm.chargeAllocation(); err != nil {
			return taskYield{}, true, err
		}
		child := &executionTask{id: childID, scope: task.scope, execution: task.execution, budget: task.budget, frames: []*executionFrame{childFrame}, debugParents: machine.debugParentSnapshot(task)}
		return taskYield{kind: taskYieldSpawn, child: child}, true, nil
	case preparedWaitableSend, preparedWaitableRecv, preparedWaitableRecvOK:
		selected := channelSelectCase{send: inst.op == preparedWaitableSend}
		var err error
		if selected.send {
			selected.channel, selected.value, err = callFrame.pop2()
		} else {
			selected.channel, err = callFrame.pop()
		}
		if err != nil {
			return taskYield{}, true, err
		}
		selection, err := machine.vm.prepareChannelSelection(task, callFrame.module, []channelSelectCase{selected})
		if err != nil {
			return taskYield{}, true, err
		}
		if !selection.tryCommit() {
			selection.register()
			machine.blockTask(task, current, pc, inst, &blockedOperation{kind: "select", selection: selection, withOK: inst.op == preparedWaitableRecvOK})
			return taskYield{kind: taskYieldBlocked}, true, nil
		}
		return taskYield{}, true, selection.deliver(callFrame, nil, inst.op == preparedWaitableRecvOK)
	case preparedCallFFI:
		payload := inst.callFFI
		args, err := callFrame.popN(payload.ArgCount)
		if err != nil {
			return taskYield{}, true, err
		}
		route, ok := args[0].Data.(string)
		if !ok {
			return taskYield{}, true, fmt.Errorf("FFI route must be a string, got %s", args[0].Type)
		}
		bytes, err := ffiPayloadBytes(callFrame.module, args[1])
		if err != nil {
			return taskYield{}, true, err
		}
		if machine.vm.ffiSession == nil {
			callFrame.push(newByteSliceValue("Slice<Uint8>", ""))
			callFrame.push(newVMValue("String", "FFI route unavailable"))
			callFrame.push(newVMValue("Int", int64(1)))
			return taskYield{}, true, nil
		}
		pending, err := machine.vm.startFFICall(task.scope, route, bytes)
		if err != nil {
			return taskYield{}, true, err
		}
		machine.blockTask(task, current, pc, inst, &blockedOperation{kind: "ffi", ffi: pending})
		return taskYield{kind: taskYieldBlocked}, true, nil
	case preparedDeferPush:
		value, err := callFrame.pop()
		if err != nil {
			return taskYield{}, true, err
		}
		ownerDepth := 0
		if inst.deferValue != nil {
			ownerDepth = inst.deferValue.OwnerDepth
		}
		if ownerDepth < 0 || ownerDepth >= len(task.frames) {
			return taskYield{}, true, fmt.Errorf("defer owner depth %d exceeds frame stack", ownerDepth)
		}
		_, ref, err := machine.vm.resolveFunctionValue(callFrame.module, value)
		if err != nil {
			return taskYield{}, true, err
		}
		owner := task.frames[len(task.frames)-1-ownerDepth]
		owner.frame.defers = append(owner.frame.defers, deferredCall{caller: callFrame.module, ref: ref})
		return taskYield{}, true, nil
	case preparedRecover:
		callFrame.push(machine.recoverValue(task))
		return taskYield{}, true, nil
	case preparedPanic:
		value, err := callFrame.pop()
		if err != nil {
			return taskYield{}, true, err
		}
		machine.startPanic(task, current, pc, value)
		return taskYield{}, true, nil
	case preparedReturn:
		payload := inst.returnValue
		values, err := callFrame.popN(payload.ResultCount)
		if err != nil {
			return taskYield{}, true, err
		}
		values, err = callFrame.normalizeReturnValues(values)
		if err != nil {
			return taskYield{}, true, err
		}
		values = callFrame.retainReturnValues(values)
		current.completion = &frameCompletion{returnValues: values}
		return taskYield{}, true, nil
	default:
		return taskYield{}, false, nil
	}
}

func (machine *executionMachine) enterDirectTailCall(task *executionTask, current *executionFrame, target *moduleInstance, function loadedFunction, payload *ir.CallPayload) error {
	callFrame := current.frame
	if payload == nil || payload.ArgCount < 0 || len(callFrame.stack) < payload.ArgCount {
		argCount := 0
		if payload != nil {
			argCount = payload.ArgCount
		}
		return fmt.Errorf("tail call stack underflow: need %d values, have %d", argCount, len(callFrame.stack))
	}
	start := len(callFrame.stack) - payload.ArgCount
	args := append([]vmValue(nil), callFrame.stack[start:]...)
	qualify := target.modulePath() != callFrame.module.modulePath()
	if qualify {
		args = callFrame.module.qualifyValuesForArgumentBoundary(args)
	}
	debugging := machine.vm.breakpointsActive.Load() || machine.vm.debugStepActive.Load() || machine.vm.hostPauseRequested.Load()
	if !debugging && len(callFrame.defers) == 0 && !qualify {
		callee, err := machine.vm.newPreparedExecutionFrame(target, function, args, nil, task.id, current.expectedResults, current.qualifyResults)
		if err != nil {
			return err
		}
		callee.resume = current.resume
		callee.abort = current.abort
		callee.deferred = current.deferred
		callee.recoveredPanic = current.recoveredPanic
		current.resume = nil
		current.abort = nil
		task.frames[len(task.frames)-1] = callee
		callFrame.recycle()
		return nil
	}
	callee, err := machine.vm.newPreparedExecutionFrame(target, function, args, nil, task.id, payload.ResultCount, qualify)
	if err != nil {
		return err
	}
	callee.resume = func(_ *executionTask, caller *executionFrame, values []vmValue) error {
		normalized, err := caller.frame.normalizeReturnValues(values)
		if err != nil {
			return err
		}
		// The callee is recycled immediately after resume returns. Preserve
		// values that still occupy the callee stack backing array.
		caller.completion = &frameCompletion{returnValues: append([]vmValue(nil), normalized...)}
		return nil
	}
	clear(callFrame.stack[start:])
	callFrame.stack = callFrame.stack[:start]
	task.frames = append(task.frames, callee)
	return nil
}
