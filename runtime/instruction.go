package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (vm *vm) executeInstruction(task *executionTask, frame *frame, inst *preparedInstruction) error {
	err := vm.executeInstructionBody(task, frame, inst)
	frame.releasePopValues()
	return err
}

func (vm *vm) executeInstructionBody(task *executionTask, frame *frame, inst *preparedInstruction) error {
	switch inst.op {
	case preparedTypeDispatch:
		dispatch := inst.dispatch
		if !frame.typeDispatchActive {
			frame.typeDispatchValue = frame.localCells[dispatch.subject].load()
			frame.typeDispatchActive = true
		}
		value := frame.typeDispatchValue
		if frame.typeDispatchIndex == 0 && dispatch.concreteEnd > 0 {
			dynamic, present, err := frame.module.unwrapDynamicInterfaceValue(value, "type switch")
			if err != nil {
				return err
			}
			key, known := typeDispatchIdentity{}, !present
			if present {
				key, known = typeDispatchIdentityOf(dynamic.Type)
			}
			if known {
				frame.typeDispatchIndex = dispatch.concreteEnd
				if index, found := dispatch.concrete[key]; found {
					frame.typeDispatchIndex = index
				}
			}
		}
		target, binding := dispatch.fallback, dispatch.fallbackLocal
		if frame.typeDispatchIndex < len(dispatch.cases) {
			match := dispatch.cases[frame.typeDispatchIndex]
			matched := isNilDynamicInterface(frame.module, value)
			selected := value
			if match.typ.Valid() {
				var err error
				selected, matched, err = frame.module.matchTypeAssertion(value, match.typ, false)
				if err != nil {
					return err
				}
			}
			if !matched {
				frame.typeDispatchIndex++
				frame.pc--
				return nil
			}
			if !match.original {
				value = selected
			}
			target, binding = match.target, match.binding
		}
		if binding >= 0 {
			if err := frame.localCells[binding].store(value); err != nil {
				return err
			}
		}
		frame.typeDispatchIndex, frame.pc = 0, target
		frame.typeDispatchValue, frame.typeDispatchActive = vmValue{}, false
		return nil
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
		if typ.ShapeKind() == types.Array || typ.ShapeKind() == types.Struct {
			// Zero is already represented by the destination's zero-filled
			// typed lanes. Do not construct a host tree merely to encode it.
			frame.push(vmValue{Type: typ})
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
		out, err := frame.module.evalUnary(inst.operator, value, inst.numericInputs[0])
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedBinary, preparedCompareBranch:
		var out vmValue
		var err error
		handled := false
		leftInfo := runtimeNumericByPrimitive[inst.numericInputs[0]].info
		rightInfo := runtimeNumericByPrimitive[inst.numericInputs[1]].info
		if frame.slotInput == 2 && (leftInfo.Kind == types.NumericSigned || leftInfo.Kind == types.NumericUnsigned) &&
			(rightInfo.Kind == types.NumericSigned || rightInfo.Kind == types.NumericUnsigned) {
			left, leftReady := frame.integerOperand(inst.operands.Inputs[0], leftInfo)
			right, rightReady := frame.integerOperand(inst.operands.Inputs[1], rightInfo)
			if leftReady && rightReady {
				leftKind, rightKind := vmScalarSigned, vmScalarSigned
				if leftInfo.Kind == types.NumericUnsigned {
					leftKind = vmScalarUnsigned
				}
				if rightInfo.Kind == types.NumericUnsigned {
					rightKind = vmScalarUnsigned
				}
				out, handled, err = evalIntegerBinary(inst.operator, inst.runtimeType, leftKind, left, rightKind, right)
				if handled {
					frame.slotInput = 0
				}
			}
		}
		if !handled {
			left, right, popErr := frame.pop2()
			if popErr != nil {
				return popErr
			}
			out, err = frame.module.evalBinary(inst.operator, left, right, [2]types.PrimitiveKind{inst.numericInputs[0], inst.numericInputs[1]})
		}
		if err != nil {
			return err
		}
		if inst.op == preparedCompareBranch {
			matched, err := truthy(out)
			if err != nil {
				return err
			}
			if matched == inst.comparison.When {
				frame.pc = inst.jumpPC
			}
		} else {
			frame.push(out)
		}
	case preparedUpdateCompareBranch:
		leftInfo := runtimeNumericByPrimitive[inst.numericInputs[0]].info
		updateInfo := runtimeNumericByPrimitive[inst.numericInputs[1]].info
		comparisonInfo := runtimeNumericByPrimitive[inst.numericInputs[2]].info
		if frame.slotInput == 3 && (leftInfo.Kind == types.NumericSigned || leftInfo.Kind == types.NumericUnsigned) &&
			(updateInfo.Kind == types.NumericSigned || updateInfo.Kind == types.NumericUnsigned) &&
			(comparisonInfo.Kind == types.NumericSigned || comparisonInfo.Kind == types.NumericUnsigned) {
			current, currentReady := frame.integerOperand(inst.operands.Inputs[0], leftInfo)
			update, updateReady := frame.integerOperand(inst.operands.Inputs[1], updateInfo)
			comparison, comparisonReady := frame.integerOperand(inst.operands.Inputs[2], comparisonInfo)
			if currentReady && updateReady && comparisonReady {
				kind := vmScalarSigned
				if leftInfo.Kind == types.NumericUnsigned {
					kind = vmScalarUnsigned
				}
				updated, handled, fastErr := evalIntegerBinary(inst.updateOperator, inst.runtimeType, kind, current, kind, update)
				if fastErr != nil {
					return fastErr
				}
				matchedValue, compared, fastErr := evalIntegerBinary(inst.operator, inst.runtimeType, kind, updated.scalar, kind, comparison)
				if fastErr != nil {
					return fastErr
				}
				localIndex := int(inst.operands.Inputs[0].Index)
				if handled && compared && localIndex >= 0 && localIndex < len(frame.localCells) {
					cell := frame.localCells[localIndex]
					if cell.directOwner == frame && cell.directIndex >= 0 && cell.directIndex < len(frame.directScalars) {
						frame.directScalars[cell.directIndex] = updated.scalar
						frame.directKinds[cell.directIndex] = updated.scalarKind
						frame.directInitialized[cell.directIndex] = true
						frame.slotInput = 0
						matched, truthErr := truthy(matchedValue)
						if truthErr != nil {
							return truthErr
						}
						if matched == inst.updateComparison.When {
							frame.pc = inst.jumpPC
						}
						break
					}
				}
			}
		}
		comparisonValue, err := frame.pop()
		if err != nil {
			return err
		}
		updateValue, err := frame.pop()
		if err != nil {
			return err
		}
		currentValue, err := frame.pop()
		if err != nil {
			return err
		}
		updated, err := frame.module.evalBinary(
			inst.updateOperator,
			currentValue,
			updateValue,
			[2]types.PrimitiveKind{inst.numericInputs[0], inst.numericInputs[1]},
		)
		if err != nil {
			return err
		}
		localIndex := int(inst.operands.Inputs[0].Index)
		if localIndex < 0 || localIndex >= len(frame.localCells) {
			return errors.New("update comparison local out of range")
		}
		if err := frame.localCells[localIndex].store(updated); err != nil {
			return err
		}
		matchedValue, err := frame.module.evalBinary(
			inst.operator,
			updated,
			comparisonValue,
			[2]types.PrimitiveKind{inst.numericInputs[0], inst.numericInputs[2]},
		)
		if err != nil {
			return err
		}
		matched, err := truthy(matchedValue)
		if err != nil {
			return err
		}
		if matched == inst.updateComparison.When {
			frame.pc = inst.jumpPC
		}
	case preparedMinMax:
		values, err := frame.popN(inst.minMax.ArgCount)
		if err != nil {
			return err
		}
		out, err := frame.module.evalMinMax(inst.minMaxKind, inst.minMax.Operator, values)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedLenCompareBranch:
		object, other, err := frame.pop2()
		if err != nil {
			return err
		}
		length, err := lengthValue(frame.module, object)
		if err != nil {
			return err
		}
		matched, err := compareLengthValue(inst.operator, length, other)
		if err != nil {
			return err
		}
		if matched == inst.comparison.When {
			frame.pc = inst.jumpPC
		}
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
	case preparedConvert, preparedIndexConvert:
		payload := inst.typeOperand
		var value vmValue
		var err error
		if inst.op == preparedIndexConvert {
			object, index, popErr := frame.pop2()
			if popErr != nil {
				return popErr
			}
			value, err = indexValue(frame.module, object, index)
		} else {
			value, err = frame.pop()
		}
		if err != nil {
			return err
		}
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		if inst.conversion.scalarTarget() {
			if out, handled, fastErr := convertPrimitiveValue(value, typ, inst.conversion); handled || fastErr != nil {
				if fastErr != nil {
					return fastErr
				}
				frame.push(out)
				return nil
			}
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
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		out, err := newSequenceValueWithBatch(frame.module, typ, values, frame.allocationBatch)
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
		out, err := newMapValueWithCapacityBatch(frame.module, typ, values, capacity, frame.allocationBatch)
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
		typ := inst.runtimeType
		if !typ.Valid() {
			typ = frame.module.runtimeType(payload.Type)
		}
		schema := inst.structSchema
		if schema == nil {
			schema, _ = frame.module.structSchema(typ)
		}
		out, err := newStructValueWithSchemaBatch(frame.module, typ, schema, payload.Fields, values, frame.allocationBatch)
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
		out, err := makeWaitableValueWithBatch(frame.module, typ, capacity, frame.allocationBatch)
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
		return frame.initMapIteratorAt(inst.localIndex, inst.local.Local, values[0])
	case preparedMapIterNext:
		return frame.nextMapIteratorAt(inst.localIndex, inst.local.Local)
	case preparedMapIterClose:
		var iterator *mapIterator
		if inst.localIndex >= 0 && inst.localIndex < len(frame.mapIteratorSlots) {
			iterator = frame.mapIteratorSlots[inst.localIndex]
		}
		if iterator != nil {
			iterator.release(vm)
		}
		if inst.localIndex >= 0 && inst.localIndex < len(frame.mapIteratorSlots) {
			frame.mapIteratorSlots[inst.localIndex] = nil
		}
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
		out, err := appendValueWithBatch(frame.module, values[0], values[1:], payload.Expand, frame.allocationBatch)
		if err != nil {
			return err
		}
		frame.push(out)
	case preparedDelete:
		object, key, err := frame.pop2()
		if err != nil {
			return err
		}
		if err := deleteValueWithBatch(frame.module, object, key, frame.allocationBatch); err != nil {
			return err
		}
	case preparedClear:
		value, err := frame.pop()
		if err != nil {
			return err
		}
		if err := clearValueWithBatch(frame.module, value, frame.allocationBatch); err != nil {
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
		_, err = setIndexValueWithBatch(frame.module, values[0], values[1], values[2], frame.allocationBatch)
		if err != nil {
			return err
		}
	case preparedGetPath:
		object, err := frame.pop()
		if err != nil {
			return err
		}
		for _, step := range inst.path {
			if step.indirect {
				object, err = derefPointer(object)
				if err != nil {
					return err
				}
			}
			storage, ok := object.payload().(*vmStruct)
			if !ok || storage == nil {
				return fmt.Errorf("field path requires a struct, got %s", object.Type)
			}
			object, ok = storage.fieldAt(step.index)
			if !ok {
				object = storage.initializeField(step.index, frame.module.zeroValue(step.typ))
			}
			if !object.Type.Equal(step.typ) {
				object, err = frame.module.convertValue(object, step.typ)
				if err != nil {
					return err
				}
			}
		}
		frame.push(object)
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
		out, ok, err := waitableTryRecvValues(frame.module, value, frame.allocationBatch)
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
		ok, err := waitableTrySendValue(frame.module, waitable, value, frame.allocationBatch)
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
		if copied, err := frame.copyLocalToOutput(inst.localIndex); copied {
			return err
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
		if payload.Rebind && inst.localIndex < len(frame.function.LocalEscapes) && frame.function.LocalEscapes[inst.localIndex] {
			cell = newSlot(cell.typ, cell.module, cell.variadic)
			if cell.module != nil && cell.module.vm != nil && cell.module.vm.heap != nil {
				if _, err := cell.ensureManagedCell(); err != nil {
					return err
				}
			}
			frame.localCells[inst.localIndex] = cell
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
		if ok != inst.jump.Negate {
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
		captureSlots := make([]*slot, len(payload.Captures))
		for i, capture := range payload.Captures {
			slot, err := frame.capture(capture)
			if err != nil {
				return err
			}
			captureSlots[i] = slot
		}
		frame.push(newVMValue("Function", functionRef{
			ModulePath:   targetModule.executable.Artifact.Module.Path,
			FunctionID:   payload.Function,
			exact:        targetModule,
			captureSlots: captureSlots,
		}))
	case preparedCallIntrinsic:
		payload := inst.callIntrinsic
		args, err := frame.popN(payload.ArgCount)
		if err != nil {
			return err
		}
		values, err := invokeIntrinsic(intrinsicContext{vm: vm, module: frame.module, task: task, batch: task.allocationBatch}, payload.ID, args)
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
