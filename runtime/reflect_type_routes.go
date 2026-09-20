package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

func reflectTypeOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.type_of expects 1 argument, got %d", len(args))
	}
	value, ok, err := reflectDynamicValue(ctx, args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return []vmValue{zeroReflectTypeValue()}, nil
	}
	info := reflectTypeInfoForValue(reflectRelationModule(ctx), value)
	return []vmValue{reflectTypeValueFromVM(ctx.vm, info)}, nil
}

func reflectTypeDescriptor(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.type_descriptor expects 1 argument, got %d", len(args))
	}
	info, err := reflectResolvedTypeInfo(ctx, args[0])
	if err != nil {
		return []vmValue{newVMValue("reflect.runtimeTypeData", nil), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{reflectTypeDescriptorValue(ctx, info), newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectTypeField(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.type_field expects 2 arguments, got %d", len(args))
	}
	index, err := asInt64(args[1])
	if err != nil {
		return []vmValue{newVMValue("reflect.StructField", nil), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	info, err := reflectResolvedTypeInfo(ctx, args[0])
	if err != nil {
		return []vmValue{newVMValue("reflect.StructField", nil), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if info.Kind != "struct" {
		message := "reflect: Field of non-struct type " + reflectTypeDisplay(info)
		return []vmValue{newVMValue("reflect.StructField", nil), newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	if index < 0 || index >= int64(len(info.Fields)) {
		message := "reflect: Field index out of bounds"
		return []vmValue{newVMValue("reflect.StructField", nil), newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	field := reflectStructFieldValueAt(ctx, info.Fields[int(index)], int(index))
	return []vmValue{field, newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectTypeMethod(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.type_method expects 2 arguments, got %d", len(args))
	}
	index, err := asInt64(args[1])
	if err != nil {
		return []vmValue{newVMValue("reflect.Method", nil), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	info, err := reflectResolvedTypeInfo(ctx, args[0])
	if err != nil {
		return []vmValue{newVMValue("reflect.Method", nil), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if index < 0 || index >= int64(len(info.Methods)) {
		message := "reflect: Method index out of range"
		return []vmValue{newVMValue("reflect.Method", nil), newVMValue("String", message), newVMValue("Bool", false)}, nil
	}
	method, err := reflectMethodValueAt(ctx, info, info.Methods[int(index)], int(index))
	if err != nil {
		return []vmValue{newVMValue("reflect.Method", nil), newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{method, newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectResolvedTypeInfo(ctx intrinsicContext, value vmValue) (TypeInfo, error) {
	key, err := reflectTypeKeyFromValue(value)
	if err != nil {
		return TypeInfo{}, err
	}
	if ctx.vm == nil {
		return TypeInfo{}, errors.New("reflect: Type requires VM context")
	}
	if info, ok := ctx.vm.reflectTypes.load(key); ok {
		return info, nil
	}
	return TypeInfo{}, fmt.Errorf("reflect: type %s is unavailable", key)
}

func reflectTypeDisplay(info TypeInfo) string {
	if info.Display != "" {
		return info.Display
	}
	if info.QualifiedName != "" {
		return info.QualifiedName
	}
	if info.Type != "" {
		return info.Type
	}
	return info.QualifiedType
}

func reflectAssignableTo(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	source, target, err := reflectTypeRelationArgs(ctx, "reflect.assignable_to", args)
	if err != nil {
		return nil, err
	}
	ok := reflectRelationModule(ctx).assignableType(source, target)
	return []vmValue{newVMValue("Bool", ok)}, nil
}

func reflectConvertibleTo(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	source, target, err := reflectTypeRelationArgs(ctx, "reflect.convertible_to", args)
	if err != nil {
		return nil, err
	}
	module := reflectRelationModule(ctx)
	ok := module.convertibleType(source, target)
	return []vmValue{newVMValue("Bool", ok)}, nil
}

func reflectImplements(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	source, target, err := reflectTypeRelationArgs(ctx, "reflect.implements", args)
	if err != nil {
		return nil, err
	}
	if target.ShapeKind() != types.Interface {
		return []vmValue{newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	ok := module.implementsInterface(source, target.String())
	return []vmValue{newVMValue("Bool", ok)}, nil
}

func reflectValueOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_of expects 1 argument, got %d", len(args))
	}
	value, ok, err := reflectDynamicValue(ctx, args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return []vmValue{zeroReflectValueValue()}, nil
	}
	out, err := reflectValueSnapshot(ctx, value)
	if err != nil {
		return nil, err
	}
	return []vmValue{out}, nil
}

func reflectValueElem(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_elem expects 1 argument, got %d", len(args))
	}
	value, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	data, ok, err := reflectCurrentValue(value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		return reflectValueError("reflect: Elem of invalid Value"), nil
	}
	module := reflectRelationModule(ctx)
	if data.Type.Ref.Kind == types.Any || module != nil && module.isInterfaceType(data.Type) {
		elem, ok, err := reflectDynamicValue(ctx, data)
		if err != nil {
			return reflectValueError(err.Error()), nil
		}
		if !ok {
			return reflectValueOK(zeroReflectValueValue()), nil
		}
		out, err := reflectValueSnapshotWithTarget(ctx, elem, vmValue{}, false, false, reflectBoolField(value.get("interfaceable")))
		if err != nil {
			return reflectValueError(err.Error()), nil
		}
		return reflectValueOK(out), nil
	}
	if _, ok := data.Type.PointerElem(); !ok {
		return reflectValueError("reflect: Elem of non-pointer Value"), nil
	}
	elem, err := derefPointer(data)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	interfaceable := reflectBoolField(value.get("interfaceable"))
	out, err := reflectValueSnapshotWithTarget(ctx, elem, data, true, interfaceable, interfaceable)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	out = reflectCopyEmbeddedReadOnly(out, value)
	return []vmValue{out, newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueAddr(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_addr expects 1 argument, got %d", len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !reflectBoolField(fields.get("addressable")) {
		return reflectValueError("reflect: Addr of unaddressable Value"), nil
	}
	target, ok, err := reflectTargetPayload(fields)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		return reflectValueError("reflect: Addr of unaddressable Value"), nil
	}
	if _, err := pointerValue(target); err != nil {
		return reflectValueError(err.Error()), nil
	}
	if _, err := reflectRegisterDynamicType(ctx, target.Type.String(), TypeInfo{}); err != nil {
		return nil, err
	}
	out, err := reflectValueSnapshotWithTarget(ctx, target, vmValue{}, false, false, reflectBoolField(fields.get("interfaceable")))
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(reflectCopyEmbeddedReadOnly(out, fields)), nil
}

func reflectValueSlice(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	return reflectValueSliceBounds(ctx, args, false)
}

func reflectValueSlice3(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	return reflectValueSliceBounds(ctx, args, true)
}

func reflectValueSliceBounds(ctx intrinsicContext, args []vmValue, full bool) ([]vmValue, error) {
	expected := 3
	if full {
		expected = 4
	}
	if len(args) != expected {
		return nil, fmt.Errorf("reflect: Slice expects %d arguments, got %d", expected-1, len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	current, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		return reflectValueError("reflect: Slice of invalid Value"), nil
	}
	start, err := asInt64(args[1])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	end, err := asInt64(args[2])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	maxValue := newVMValue("Void", nil)
	if full {
		maxValue = args[3]
	}
	module := reflectRelationModule(ctx)
	if module == nil {
		return reflectValueError("reflect: Slice requires module context"), nil
	}
	if module.isArrayType(current.Type) && !module.isSliceType(current.Type) && !reflectBoolField(fields.get("addressable")) {
		return reflectValueError("reflect: Slice of unaddressable array Value"), nil
	}
	_, isString := current.Data.(string)
	if full && isString {
		return reflectValueError("reflect: Slice3 of string Value"), nil
	}
	view, err := sliceValue(module, current, newVMValue("Int", start), newVMValue("Int", end), maxValue)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if isString {
		out, err := reflectValueSnapshotWithTarget(ctx, view, vmValue{}, false, false, reflectBoolField(fields.get("interfaceable")))
		if err != nil {
			return reflectValueError(err.Error()), nil
		}
		return reflectValueOK(out), nil
	}
	identity := fmt.Sprintf("reflect-slice:%s:%d:%d", current.Type, start, end)
	if full {
		maxIndex, _ := asInt64(maxValue)
		identity = fmt.Sprintf("%s:%d", identity, maxIndex)
	}
	cell := view
	ptr := reflectCellPointer(module, view.Type.String(), identity, cell)
	out, err := reflectValueSnapshotWithTarget(ctx, view, ptr, false, false, reflectBoolField(fields.get("interfaceable")))
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectValueCurrent(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.value_current expects 1 argument, got %d", len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	current, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		return reflectValueError("reflect: current of invalid Value"), nil
	}
	if target, targetOK, err := reflectTargetPayload(fields); targetOK || err != nil {
		if err != nil {
			return reflectValueError(err.Error()), nil
		}
		out, err := reflectValueSnapshotWithTarget(ctx, current, target, reflectBoolField(fields.get("addressable")), reflectBoolField(fields.get("settable")), reflectBoolField(fields.get("interfaceable")))
		if err != nil {
			return reflectValueError(err.Error()), nil
		}
		return reflectValueOK(reflectCopyEmbeddedReadOnly(out, fields)), nil
	}
	out, err := reflectValueSnapshotWithTarget(ctx, current, vmValue{}, false, false, reflectBoolField(fields.get("interfaceable")))
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(reflectCopyEmbeddedReadOnly(out, fields)), nil
}

func reflectValueConvert(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_convert expects 2 arguments, got %d", len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	current, ok, err := reflectCurrentValue(fields)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if !ok {
		return reflectValueError("reflect: Convert of invalid Value"), nil
	}
	target, err := reflectRuntimeTypeFromTypeValue(ctx, args[1])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	converted, err := reflectRelationModule(ctx).convertValue(current, target)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	out, err := reflectValueSnapshot(ctx, converted)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}
