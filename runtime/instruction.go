package runtime

import (
	"errors"
	"fmt"
)

func (vm *vm) executeInstruction(task *executionTask, frame *frame, inst *preparedInstruction) error {
	err := vm.executeInstructionBody(task, frame, inst)
	if _, census := findGuestCensusRequest(err); !census {
		frame.releasePopValues()
	}
	return err
}

func (vm *vm) executeInstructionBody(task *executionTask, frame *frame, inst *preparedInstruction) error {
	switch inst.op {
	case preparedPanic, preparedDeferPush, preparedRecover, preparedLoadExport, preparedInitModule,
		preparedCallDirect, preparedTailCallDirect, preparedCallValue, preparedCallInterface, preparedSpawn,
		preparedWaitableSend, preparedWaitableRecv, preparedWaitableRecvOK,
		preparedCallFFI, preparedReturn:
		return fmt.Errorf("control opcode %q reached non-scheduler instruction path", inst.opcodeText())
	case preparedConst:
		value, err := frame.module.constantValueAt(inst.constantIndex)
		if err != nil {
			return err
		}
		frame.push(value)
	case preparedZero:
		payload := inst.typeOperand
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		if inst.structSchema != nil {
			frame.push(newVMValue(typ, &vmStruct{schema: inst.structSchema}))
		} else {
			frame.push(frame.module.zeroValue(typ))
		}
	case preparedPop:
		_, err := frame.pop()
		return err
	case preparedUnary:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		out, err := frame.module.evalUnary(inst.operator, value)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedBinary:
		left, right, err := frame.pop2()
		if err != nil {
			return err
		}
		out, err := frame.module.evalBinary(inst.operator, left, right)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedTypeAssert:
		payload := inst.typeOperand
		value, err := frame.pop()
		if err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := frame.module.typeAssertionValueWithVariadic(value, typ, inst.typeVariadic)
		if err != nil {
			return newGuestPanic(err)
		}
		frame.push(out)
	case preparedTypeAssertOK:
		payload := inst.typeOperand
		value, err := frame.pop()
		if err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, ok, err := frame.module.typeAssertOKValueWithVariadic(value, typ, inst.typeVariadic)
		if err != nil {
			return err
		}
		frame.push(out)
		frame.push(ok)
	case preparedConvert:
		payload := inst.typeOperand
		value, err := frame.pop()
		if err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := frame.module.convertValueWithVariadic(value, typ, inst.typeVariadic)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedMakeSequence:
		payload := inst.makeSequence
		values, err := frame.popN(payload.ElementCount)
		if err != nil {
			return err
		}
		if err := vm.chargeRuntimeObject(len(values), 0); err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := newSequenceValue(frame.module, typ, values)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedMakeMap:
		payload := inst.makeMap
		valueCount := payload.EntryCount * 2
		popCount := valueCount
		if payload.HasCapacity {
			popCount++
		}
		values, err := frame.popN(popCount)
		if err != nil {
			return err
		}
		capacityValue := int64(valueCount / 2)
		if payload.HasCapacity {
			requested := values[valueCount]
			capacityInt, err := asInt64(requested)
			if err != nil {
				return fmt.Errorf("map size: %w", err)
			}
			capacityValue = capacityInt
			values = values[:valueCount]
		}
		_, capacity, err := vm.checkCollectionSize(0, capacityValue)
		if err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := newMapValueWithCapacity(frame.module, typ, values, capacity)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedMakeStruct:
		payload := inst.makeStruct
		values, err := frame.popN(len(payload.Fields))
		if err != nil {
			return err
		}
		if err := vm.chargeRuntimeObject(len(payload.Fields), 0); err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		schema := inst.structSchema
		if schema == nil {
			schema, _ = frame.module.structSchema(typ)
		}
		out, err := newStructValueWithSchema(frame.module, typ, schema, payload.Fields, values)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedMakeSlice:
		payload := inst.makeSlice
		count := 1
		if payload.HasCapacity {
			count = 2
		}
		values, err := frame.popN(count)
		if err != nil {
			return err
		}
		capacity := vmValue{}
		if payload.HasCapacity {
			capacity = values[1]
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := makeSliceValue(frame.module, typ, values[0], capacity)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedMakeWaitable:
		payload := inst.makeWaitable
		capacity, err := frame.pop()
		if err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := makeWaitableValue(frame.module, typ, capacity)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedLoadIndex:
		object, index, err := frame.pop2()
		if err != nil {
			return err
		}
		out, err := indexValue(frame.module, object, index)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedLoadIndexOK:
		object, index, err := frame.pop2()
		if err != nil {
			return err
		}
		out, ok, err := mapIndexOKValues(frame.module, object, index)
		if err != nil {
			return err
		}
		frame.push(out)
		frame.push(ok)
	case preparedStringRuneAt:
		text, index, err := frame.pop2()
		if err != nil {
			return err
		}
		out, err := stringRuneAtValue(text, index)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedStringNextRuneIndex:
		text, index, err := frame.pop2()
		if err != nil {
			return err
		}
		out, err := stringNextIndexValue(text, index)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedSlice:
		values, err := frame.popN(4)
		if err != nil {
			return err
		}
		out, err := sliceValue(frame.module, values[0], values[1], values[2], values[3])
		if err != nil {
			return newGuestPanic(err)
		}
		frame.push(out)
	case preparedLen:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		out, err := lenValue(frame.module, value)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedCap:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		out, err := capValue(frame.module, value)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedMapIterInit:
		values, err := frame.popN(1)
		if err != nil {
			return err
		}
		return frame.initMapIterator(inst.local.Local, values[0])
	case preparedMapIterNext:
		return frame.nextMapIterator(inst.local.Local)
	case preparedMapIterClose:
		delete(frame.mapIterators, inst.local.Local)
	case preparedMapKeys:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		out, err := mapKeysValue(frame.module, value)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedAppend:
		payload := inst.count
		values, err := frame.popN(payload.Count + 1)
		if err != nil {
			return err
		}
		out, err := appendValue(frame.module, values[0], values[1:], payload.Expand)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedDelete:
		object, key, err := frame.pop2()
		if err != nil {
			return err
		}
		if err := deleteValue(frame.module, object, key); err != nil {
			return err
		}
	case preparedClear:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		if err := clearValue(frame.module, value); err != nil {
			return err
		}
	case preparedCopy:
		destination, source, err := frame.pop2()
		if err != nil {
			return err
		}
		out, err := copyValue(frame.module, destination, source)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedStoreIndex:
		values, err := frame.popN(3)
		if err != nil {
			return err
		}
		_, err = setIndexValue(frame.module, values[0], values[1], values[2])
		if err != nil {
			return err
		}
	case preparedLoadField:
		payload := inst.field
		object, err := frame.pop()
		if err != nil {
			return err
		}
		out, err := loadFieldValue(frame.module, object, payload.Field)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedStoreField:
		payload := inst.field
		object, value, err := frame.pop2()
		if err != nil {
			return err
		}
		_, err = storeFieldValue(frame.module, object, payload.Field, value)
		if err != nil {
			return err
		}
	case preparedAddressOf:
		payload := inst.address
		if err := vm.chargeAllocation(); err != nil {
			return err
		}
		value, err := frame.address(*payload)
		if err != nil {
			return err
		}
		frame.push(value)
	case preparedLoadIndirect:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		out, err := derefPointer(value)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedStoreIndirect:
		pointer, value, err := frame.pop2()
		if err != nil {
			return err
		}
		if err := storePointer(pointer, value); err != nil {
			return err
		}
	case preparedWaitableTryRecv:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		out, ok, err := waitableTryRecvValues(frame.module, value)
		if err != nil {
			return err
		}
		frame.push(out)
		frame.push(ok)
	case preparedWaitableCanRecv:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		ready, err := waitableReadyRecvValue(frame.module, value)
		if err != nil {
			return err
		}
		frame.push(ready)
	case preparedWaitableTrySend:
		waitable, value, err := frame.pop2()
		if err != nil {
			return err
		}
		ok, err := waitableTrySendValue(frame.module, waitable, value)
		if err != nil {
			return err
		}
		frame.push(newVMValue("Bool", ok))
	case preparedWaitableCanSend:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		ready, err := waitableReadySendValue(frame.module, value)
		if err != nil {
			return err
		}
		frame.push(ready)
	case preparedWaitableClose:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		return waitableCloseValue(frame.module, value)
	case preparedLoadLocal:
		payload := inst.local
		if inst.localIndex < 0 || inst.localIndex >= len(frame.localCells) {
			return fmt.Errorf("unknown local %q", payload.Local)
		}
		slot := frame.localCells[inst.localIndex]
		frame.push(slot.load())
	case preparedStoreLocal:
		payload := inst.local
		value, err := frame.pop()
		if err != nil {
			return err
		}
		if inst.localIndex < 0 || inst.localIndex >= len(frame.localCells) {
			return fmt.Errorf("unknown local %q", payload.Local)
		}
		cell := frame.localCells[inst.localIndex]
		if payload.Rebind {
			if inst.localIndex < len(frame.function.LocalEscapes) && frame.function.LocalEscapes[inst.localIndex] && cell != &frame.localStorage[inst.localIndex] {
				cell = newSlot(cell.typ, cell.module, cell.variadic)
				frame.localCells[inst.localIndex] = cell
			} else {
				*cell = slot{typ: cell.typ, module: cell.module, variadic: cell.variadic}
			}
		}
		if err := cell.store(value); err != nil {
			return fmt.Errorf("store local %s: %w", payload.Local, err)
		}
	case preparedLoadUpvalue:
		payload := inst.upvalue
		if inst.upvalueIndex < 0 || inst.upvalueIndex >= len(frame.upvalueCells) || frame.upvalueCells[inst.upvalueIndex] == nil {
			return fmt.Errorf("unknown upvalue %q", payload.Upvalue)
		}
		slot := frame.upvalueCells[inst.upvalueIndex]
		frame.push(slot.load())
	case preparedStoreUpvalue:
		payload := inst.upvalue
		value, err := frame.pop()
		if err != nil {
			return err
		}
		if inst.upvalueIndex < 0 || inst.upvalueIndex >= len(frame.upvalueCells) || frame.upvalueCells[inst.upvalueIndex] == nil {
			return fmt.Errorf("unknown upvalue %q", payload.Upvalue)
		}
		slot := frame.upvalueCells[inst.upvalueIndex]
		if err := slot.store(value); err != nil {
			return fmt.Errorf("store upvalue %s: %w", payload.Upvalue, err)
		}
	case preparedLoadGlobal:
		payload := inst.global
		if inst.globalIndex < 0 || inst.globalIndex >= len(frame.module.globalCells) {
			return fmt.Errorf("unknown global %q", payload.Global)
		}
		slot := frame.module.globalCells[inst.globalIndex]
		frame.push(slot.load())
	case preparedStoreGlobal:
		payload := inst.global
		value, err := frame.pop()
		if err != nil {
			return err
		}
		if inst.globalIndex < 0 || inst.globalIndex >= len(frame.module.globalCells) {
			return fmt.Errorf("unknown global %q", payload.Global)
		}
		slot := frame.module.globalCells[inst.globalIndex]
		if err := slot.store(value); err != nil {
			return fmt.Errorf("store global %s: %w", payload.Global, err)
		}
	case preparedJump:
		frame.pc = inst.jumpPC
		return nil
	case preparedJumpIf:
		condition, err := frame.pop()
		if err != nil {
			return err
		}
		ok, err := truthy(condition)
		if err != nil {
			return err
		}
		if ok {
			frame.pc = inst.jumpPC
		}
	case preparedMakeClosure:
		payload := inst.closure
		if payload.Function == "" {
			return errors.New("make_closure missing function")
		}
		targetModule, err := vm.directCallModule(frame.module, payload.ModulePath)
		if err != nil {
			return err
		}
		target, ok := targetModule.executable.Functions[payload.Function]
		if !ok {
			return fmt.Errorf("unknown function %q", payload.Function)
		}
		if len(payload.Captures) != len(target.Decl.Upvalues) {
			return fmt.Errorf("closure %s capture count mismatch: got %d, want %d", payload.Function, len(payload.Captures), len(target.Decl.Upvalues))
		}
		if err := vm.chargeRuntimeObject(len(payload.Captures), 0); err != nil {
			return err
		}
		upvalues := make(map[string]*slot, len(payload.Captures))
		for i, capture := range payload.Captures {
			slot, err := frame.capture(capture)
			if err != nil {
				return err
			}
			upvalues[target.Decl.Upvalues[i].ID] = slot
		}
		var exact *moduleInstance
		if len(payload.Captures) != 0 || !isLogicalFunction(target) {
			exact = targetModule
		}
		frame.push(newVMValue("Function", functionRef{
			ModulePath: targetModule.executable.Artifact.Module.Path,
			FunctionID: payload.Function,
			exact:      exact,
			upvalues:   upvalues,
		}))
	case preparedCallIntrinsic:
		payload := inst.callIntrinsic
		args, err := frame.popN(payload.ArgCount)
		if err != nil {
			return err
		}
		values, err := invokeIntrinsic(intrinsicContext{vm: vm, module: frame.module, task: task}, payload.ID, args)
		if err != nil {
			var request *artifactCallbackRequest
			if errors.As(err, &request) {
				request.resumeResults = payload.ResultCount
				return request
			}
			return err
		}
		if len(values) != payload.ResultCount {
			return fmt.Errorf("intrinsic %q returned %d values, want %d", payload.ID, len(values), payload.ResultCount)
		}
		for _, value := range values {
			frame.push(value)
		}
	default:
		return fmt.Errorf("unsupported opcode %q", inst.opcodeText())
	}
	return nil
}
