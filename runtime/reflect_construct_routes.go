package runtime

import (
	"errors"
	"fmt"
)

func reflectZero(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.zero expects 1 argument, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	value := reflectZeroValue(reflectRelationModule(ctx), typ)
	out, err := reflectValueSnapshot(ctx, value)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectTypePointerTo(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.type_pointer_to expects 1 argument, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	return reflectRegisterDynamicType(ctx, "Ptr<"+typ+">", TypeInfo{})
}

func reflectTypeSliceOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.type_slice_of expects 1 argument, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	return reflectRegisterDynamicType(ctx, "Slice<"+typ+">", TypeInfo{})
}

func reflectTypeMapOf(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.type_map_of expects 2 arguments, got %d", len(args))
	}
	keyType, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	elemType, err := reflectRuntimeTypeFromTypeValue(ctx, args[1])
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	module := reflectRelationModule(ctx)
	ok, err := module.isComparableRuntimeType(keyType, map[string]struct{}{})
	if err != nil {
		return reflectTypeError(err.Error()), nil
	}
	if !ok {
		return reflectTypeError("reflect: MapOf with uncomparable key type"), nil
	}
	return reflectRegisterDynamicType(ctx, "Map<"+keyType+", "+elemType+">", TypeInfo{})
}

func reflectNew(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.new expects 1 argument, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	module := reflectRelationModule(ctx)
	if _, err := reflectRegisterDynamicType(ctx, "Ptr<"+typ+">", TypeInfo{}); err != nil {
		return nil, err
	}
	cell := reflectZeroValue(module, typ)
	ptr := reflectCellPointer(module, typ, "reflect-new:"+typ, cell)
	out, err := reflectValueSnapshot(ctx, ptr)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectMakeSlice(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("reflect.make_slice expects 3 arguments, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	length, err := asInt64(args[1])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	capacity, err := asInt64(args[2])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	if length < 0 || capacity < length {
		return reflectValueError(fmt.Sprintf("reflect: invalid slice length/capacity %d/%d", length, capacity)), nil
	}
	module := reflectRelationModule(ctx)
	if !module.isSliceType(typ) {
		return reflectValueError("reflect: MakeSlice of non-slice type"), nil
	}
	lengthValue, capacityValue, err := ctx.vm.checkCollectionSize(length, capacity)
	if err != nil {
		var limit ResourceLimitError
		if errors.As(err, &limit) {
			return nil, err
		}
		return reflectValueError(err.Error()), nil
	}
	if ctx.vm != nil {
		if err := ctx.vm.chargeRuntimeObject(capacityValue, 0); err != nil {
			return nil, err
		}
	}
	elemType := module.arrayElemType(typ)
	backing := make([]vmValue, capacityValue)
	for i := range backing {
		backing[i] = reflectZeroValue(module, elemType)
	}
	cell := newSliceHeaderValue(typ, backing, 0, lengthValue, capacityValue)
	ptr := reflectCellPointer(module, typ, "reflect-make-slice:"+typ, cell)
	out, err := reflectValueSnapshotWithTarget(ctx, cell, ptr, true, true, true)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}

func reflectMakeMap(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("reflect.make_map expects 1 argument, got %d", len(args))
	}
	typ, err := reflectRuntimeTypeFromTypeValue(ctx, args[0])
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	module := reflectRelationModule(ctx)
	if !module.isMapType(typ) {
		return reflectValueError("reflect: MakeMap of non-map type"), nil
	}
	cell, err := newMapValue(module, typ, nil)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	ptr := reflectCellPointer(module, typ, "reflect-make-map:"+typ, cell)
	out, err := reflectValueSnapshotWithTarget(ctx, cell, ptr, true, true, true)
	if err != nil {
		return reflectValueError(err.Error()), nil
	}
	return reflectValueOK(out), nil
}
