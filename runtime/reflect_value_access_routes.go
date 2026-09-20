package runtime

import "fmt"

func reflectCurrentValueArg(value vmValue, op string) (vmValue, bool, string) {
	fields, err := reflectValuePayload(value)
	if err != nil {
		return vmValue{}, false, err.Error()
	}
	current, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return vmValue{}, false, err.Error()
	}
	if !ok {
		return vmValue{}, false, "reflect: " + op + " of invalid Value"
	}
	return current, true, ""
}

func reflectCurrentValueSliceArg(value vmValue, op string) ([]vmValue, bool, string) {
	items, ok := sliceValues(value)
	if !ok {
		return nil, false, "reflect: " + op + " expects []Value"
	}
	out := make([]vmValue, 0, len(items))
	for i, item := range items {
		current, ok, message := reflectCurrentValueArg(item, op)
		if !ok {
			return nil, false, fmt.Sprintf("reflect: %s argument %d: %s", op, i, message)
		}
		out = append(out, current)
	}
	return out, true, ""
}

func reflectValueFieldByIndex(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_field_by_index expects 2 arguments, got %d", len(args))
	}
	value, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	index, err := reflectIndexPath(args[1])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	target, ok, err := reflectTargetPayload(value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		current, currentOK, currentErr := reflectCurrentValue(value)
		if currentErr != nil {
			return reflectValueError(currentErr.Error()), nil
		}
		if !currentOK {
			return reflectValueError("reflect: FieldByIndex of invalid Value"), nil
		}
		module := reflectRelationModule(ctx)
		var fields []TypeFieldInfo
		interfaceable := reflectBoolField(value.get("interfaceable")) || reflectBoolField(value.get("embeddedReadOnly"))
		fieldInterfaceable := interfaceable
		embeddedReadOnly := false
		for pathIndex, fieldIndex := range index {
			if len(fields) == 0 {
				fields = reflectTypeInfoForValue(module, current).Fields
			}
			if fieldIndex < 0 || fieldIndex >= len(fields) {
				return reflectValueError(fmt.Sprintf("reflect: field index out of range: %d", fieldIndex)), nil
			}
			field := fields[fieldIndex]
			fieldInterfaceable = interfaceable && field.Exported
			embeddedReadOnly = interfaceable && !field.Exported && field.Embedded
			if !field.Exported && !field.Embedded {
				interfaceable = false
			}
			current, err = loadFieldValue(module, current, field.Name)
			if err != nil {
				return reflectValueError(err.Error()), nil
			}
			if pathIndex+1 < len(index) {
				fields = reflectTypeInfoForValue(module, current).Fields
			}
		}
		out, err := reflectValueSnapshotWithTarget(ctx, current, vmValue{}, false, false, fieldInterfaceable)
		if err != nil {
			return reflectValueError(err.Error()), nil
		}
		setRuntimeStructField(out.Data.(*vmStruct), "embeddedReadOnly", newBoolValue(embeddedReadOnly))
		return []vmValue{out, newVMValue("String", ""), newVMValue("Bool", true)}, nil
	}
	fieldPtr, interfaceable, embeddedReadOnly, err := reflectFieldPointerByIndex(reflectRelationModule(ctx), target, index, nil, reflectBoolField(value.get("interfaceable")) || reflectBoolField(value.get("embeddedReadOnly")))
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	fieldValue, err := derefPointer(fieldPtr)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	out, err := reflectValueSnapshotWithTarget(ctx, fieldValue, fieldPtr, true, interfaceable, interfaceable)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	setRuntimeStructField(out.Data.(*vmStruct), "embeddedReadOnly", newBoolValue(embeddedReadOnly))
	return []vmValue{out, newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueIndex(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_index expects 2 arguments, got %d", len(args))
	}
	value, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	index, err := asInt64(args[1])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	target, ok, err := reflectTargetPayload(value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		current, currentOK, currentErr := reflectCurrentValue(value)
		if currentErr != nil {
			return reflectValueError(currentErr.Error()), nil
		}
		if !currentOK {
			return reflectValueError("reflect: Index of invalid Value"), nil
		}
		module := reflectRelationModule(ctx)
		if module != nil && module.isSliceType(current.Type) {
			cell := current
			pointer := reflectCellPointer(module, current.Type.String(), fmt.Sprintf("reflect-slice-index:%p", current.Data), cell)
			indexPtr, indexErr := reflectIndexPointer(module, pointer, index)
			if indexErr != nil {
				return reflectValueError(indexErr.Error()), nil
			}
			item, indexErr := derefPointer(indexPtr)
			if indexErr != nil {
				return reflectValueError(indexErr.Error()), nil
			}
			interfaceable := reflectBoolField(value.get("interfaceable"))
			out, snapshotErr := reflectValueSnapshotWithTarget(ctx, item, indexPtr, true, interfaceable, interfaceable)
			if snapshotErr != nil {
				return reflectValueError(snapshotErr.Error()), nil
			}
			return reflectValueOK(out), nil
		}
		item, indexErr := indexValue(module, current, args[1])
		if indexErr != nil {
			return reflectValueError(indexErr.Error()), nil
		}
		out, snapshotErr := reflectValueSnapshot(ctx, item)
		if snapshotErr != nil {
			return reflectValueError(snapshotErr.Error()), nil
		}
		return []vmValue{out, newVMValue("String", ""), newVMValue("Bool", true)}, nil
	}
	indexPtr, err := reflectIndexPointer(reflectRelationModule(ctx), target, index)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	item, err := derefPointer(indexPtr)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	interfaceable := reflectBoolField(value.get("interfaceable"))
	out, err := reflectValueSnapshotWithTarget(ctx, item, indexPtr, true, interfaceable, interfaceable)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return []vmValue{out, newVMValue("String", ""), newVMValue("Bool", true)}, nil
}
