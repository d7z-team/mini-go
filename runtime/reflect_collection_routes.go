package runtime

import "fmt"

func reflectValueAppend(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_append expects 2 arguments, got %d", len(args))
	}
	target, ok, message := reflectCurrentValueArg(args[0], "Append")
	if !ok {
		return reflectValueError(message), nil
	}
	valueItems, ok, message := reflectCurrentValueSliceArg(args[1], "Append")
	if !ok {
		return reflectValueError(message), nil
	}
	module := reflectRelationModule(ctx)
	appended, err := appendValue(module, target, valueItems, false)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	out, err := reflectValueSnapshot(ctx, appended)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectValueAppendSlice(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_append_slice expects 2 arguments, got %d", len(args))
	}
	target, ok, message := reflectCurrentValueArg(args[0], "AppendSlice")
	if !ok {
		return reflectValueError(message), nil
	}
	source, ok, message := reflectCurrentValueArg(args[1], "AppendSlice")
	if !ok {
		return reflectValueError(message), nil
	}
	module := reflectRelationModule(ctx)
	appended, err := appendValue(module, target, []vmValue{source}, true)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	out, err := reflectValueSnapshot(ctx, appended)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectValueCopy(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_copy expects 2 arguments, got %d", len(args))
	}
	dst, ok, message := reflectCurrentValueArg(args[0], "Copy")
	if !ok {
		return []vmValue{newVMValue("Int", int64(0)), newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	src, ok, message := reflectCurrentValueArg(args[1], "Copy")
	if !ok {
		return []vmValue{newVMValue("Int", int64(0)), newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	count, err := copyValue(reflectRelationModule(ctx), dst, src)
	if err != nil {
		return []vmValue{newVMValue("Int", int64(0)), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{count, newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueSwap(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("reflect.value_swap expects 3 arguments, got %d", len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	left, err := asInt64(args[1])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	right, err := asInt64(args[2])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	value, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !ok {
		return []vmValue{newVMValue("String", "reflect: Swapper of invalid Value"), newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isSliceType(value.Type) {
		return []vmValue{newVMValue("String", "reflect: Swapper of non-slice Value"), newVMValue("Bool", false)}, nil
	}
	slice, ok := value.payload().(*vmSlice)
	if !ok {
		return []vmValue{newVMValue("String", fmt.Sprintf("reflect: invalid slice backing for %s", value.Type)), newVMValue("Bool", false)}, nil
	}
	if slice == nil || left < 0 || right < 0 || int(left) >= slice.Len || int(right) >= slice.Len {
		return []vmValue{newVMValue("String", "reflect: slice index out of range"), newVMValue("Bool", false)}, nil
	}
	if left != right {
		if slice.managedHeap != nil && slice.managedRef.valid() {
			stride := slice.managedElem.Stride
			leftOffset := slice.managedBase + uint32(slice.Start+int(left))*stride
			rightOffset := slice.managedBase + uint32(slice.Start+int(right))*stride
			if err := slice.managedHeap.swap(slice.managedRef, leftOffset, rightOffset, stride); err != nil {
				return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
			}
			return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
		}
		leftValue := module.cloneValueForStore(slice.valueAt(int(left)))
		rightValue := slice.valueAt(int(right))
		if err := slice.setValueAt(int(left), rightValue); err != nil {
			return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
		}
		if err := slice.setValueAt(int(right), leftValue); err != nil {
			return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
		}
	}
	return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
}
