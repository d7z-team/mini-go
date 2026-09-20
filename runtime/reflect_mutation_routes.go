package runtime

import (
	"errors"
	"fmt"
	"strings"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func reflectValueSetLen(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	return reflectValueSetSliceHeader(ctx, args, "reflect.value_set_len", "SetLen", true)
}

func reflectValueSetCap(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	return reflectValueSetSliceHeader(ctx, args, "reflect.value_set_cap", "SetCap", false)
}

func reflectValueGrow(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_grow expects 2 arguments, got %d", len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !reflectBoolField(fields.get("settable")) {
		return []vmValue{newVMValue("String", "reflect: Grow of unsettable Value"), newVMValue("Bool", false)}, nil
	}
	count, err := asInt64(args[1])
	if err != nil || count < 0 {
		return []vmValue{newVMValue("String", "reflect: Grow: negative len"), newVMValue("Bool", false)}, nil
	}
	target, ok, err := reflectTargetPayload(fields)
	if err != nil || !ok {
		return []vmValue{newVMValue("String", "reflect: Grow of unsettable Value"), newVMValue("Bool", false)}, nil
	}
	current, err := derefPointer(target)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	if module == nil || !module.isSliceType(current.Type) {
		return []vmValue{newVMValue("String", "reflect: Grow of non-slice Value"), newVMValue("Bool", false)}, nil
	}
	slice, ok := current.Data.(*vmSlice)
	if !ok && current.Data != nil {
		return []vmValue{newVMValue("String", "reflect: invalid slice backing"), newVMValue("Bool", false)}, nil
	}
	length, capacity := 0, 0
	if slice != nil {
		length, capacity = slice.Len, slice.Cap
	}
	if count <= int64(capacity-length) {
		return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
	}
	newCapacity := int64(length) + count
	if doubled := int64(capacity) * 2; doubled > newCapacity {
		newCapacity = doubled
	}
	if newCapacity < 1 {
		newCapacity = 1
	}
	_, checkedCapacity, err := ctx.vm.checkCollectionSize(int64(length), newCapacity)
	if err != nil {
		var limit ResourceLimitError
		if errors.As(err, &limit) {
			return nil, err
		}
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if ctx.vm != nil {
		if err := ctx.vm.chargeAllocationBytes(int64(checkedCapacity-capacity) * ir.RuntimeSlotBytes); err != nil {
			return nil, err
		}
	}
	var grown vmValue
	if slice != nil && slice.ByteBacked {
		backing := make([]byte, checkedCapacity)
		copy(backing, slice.bytes())
		grown = newByteSliceHeaderValue(current.Type, backing, length, checkedCapacity)
	} else {
		backing := make([]vmValue, checkedCapacity)
		if slice != nil {
			copy(backing, slice.values())
		}
		elemType := module.arrayElemType(current.Type)
		for i := length; i < len(backing); i++ {
			backing[i] = module.zeroValue(elemType)
		}
		grown = newSliceHeaderValue(current.Type, backing, 0, length, checkedCapacity)
	}
	if err := storePointer(target, grown); err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueSetSliceHeader(ctx intrinsicContext, args []vmValue, routeID, op string, setLen bool) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("%s expects 2 arguments, got %d", routeID, len(args))
	}
	fields, err := reflectValuePayload(args[0])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !reflectBoolField(fields.get("settable")) {
		return []vmValue{newVMValue("String", "reflect: "+op+" of unsettable Value"), newVMValue("Bool", false)}, nil
	}
	targetPtr, ok, err := reflectTargetPayload(fields)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !ok {
		return []vmValue{newVMValue("String", "reflect: "+op+" of unsettable Value"), newVMValue("Bool", false)}, nil
	}
	n, err := asInt64(args[1])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	current, err := derefPointer(targetPtr)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	if !module.isSliceType(current.Type) {
		return []vmValue{newVMValue("String", "reflect: "+op+" of non-slice Value"), newVMValue("Bool", false)}, nil
	}
	updated, err := reflectSetSliceHeaderValue(current, n, setLen)
	if err != nil {
		return []vmValue{newVMValue("String", "reflect: "+op+": "+err.Error()), newVMValue("Bool", false)}, nil
	}
	if err := storePointer(targetPtr, updated); err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectSetSliceHeaderValue(value vmValue, n int64, setLen bool) (vmValue, error) {
	if n < 0 {
		return vmValue{}, fmt.Errorf("negative count %d", n)
	}
	slice, ok := value.Data.(*vmSlice)
	if !ok {
		return vmValue{}, fmt.Errorf("invalid slice backing for %s", value.Type)
	}
	length := 0
	capacity := 0
	start := 0
	if slice != nil {
		length = slice.Len
		capacity = slice.Cap
		start = slice.Start
	}
	if setLen {
		if int(n) > capacity {
			return vmValue{}, fmt.Errorf("length %d exceeds capacity %d", n, capacity)
		}
		length = int(n)
	} else {
		if int(n) < length {
			return vmValue{}, fmt.Errorf("capacity %d below length %d", n, length)
		}
		if int(n) > capacity {
			return vmValue{}, fmt.Errorf("capacity %d exceeds current capacity %d", n, capacity)
		}
		capacity = int(n)
	}
	return newSliceViewValue(value.Type.String(), slice, start, length, capacity), nil
}

func reflectValueSet(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("reflect.value_set expects 2 arguments, got %d", len(args))
	}
	target, err := reflectValuePayload(args[0])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !reflectBoolField(target.get("settable")) {
		return []vmValue{newVMValue("String", "reflect: Set of unsettable Value"), newVMValue("Bool", false)}, nil
	}
	targetPtr, ok, err := reflectTargetPayload(target)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !ok {
		return []vmValue{newVMValue("String", "reflect: Set of unsettable Value"), newVMValue("Bool", false)}, nil
	}
	source, err := reflectValuePayload(args[1])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	sourceValue, ok, err := reflectCurrentValue(source)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !ok {
		return []vmValue{newVMValue("String", "reflect: Set from invalid Value"), newVMValue("Bool", false)}, nil
	}
	pointer, err := pointerValue(targetPtr)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	valueToStore := sourceValue
	if ctx.module != nil {
		converted, convertErr := ctx.module.convertValue(sourceValue, pointer.Type)
		if convertErr == nil {
			valueToStore = converted
		}
	}
	if err := pointer.storeValue(valueToStore, false); err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueSetMapIndex(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("reflect.value_set_map_index expects 3 arguments, got %d", len(args))
	}
	target, err := reflectValuePayload(args[0])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !reflectBoolField(target.get("interfaceable")) {
		return []vmValue{newVMValue("String", "reflect: SetMapIndex using value obtained from unexported field"), newVMValue("Bool", false)}, nil
	}
	targetPtr, hasTarget, err := reflectTargetPayload(target)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	targetMap, ok, err := reflectCurrentValue(target)
	if err != nil || !ok {
		return []vmValue{newVMValue("String", "reflect: SetMapIndex of invalid Value"), newVMValue("Bool", false)}, nil
	}
	key, err := reflectValuePayload(args[1])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	keyValue, ok, err := reflectCurrentValue(key)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if !ok {
		return []vmValue{newVMValue("String", "reflect: SetMapIndex with invalid key"), newVMValue("Bool", false)}, nil
	}
	elem, err := reflectValuePayload(args[2])
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	elemValue, elemOK, err := reflectCurrentValue(elem)
	if err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	module := reflectRelationModule(ctx)
	if !elemOK {
		data, ok := targetMap.Data.(*vmMap)
		if !ok || data == nil {
			return []vmValue{newVMValue("String", "reflect: SetMapIndex on nil map"), newVMValue("Bool", false)}, nil
		}
		keyType, _, ok := module.mapKeyValueTypes(targetMap.Type)
		if !ok {
			return []vmValue{newVMValue("String", "reflect: SetMapIndex of non-map Value"), newVMValue("Bool", false)}, nil
		}
		keyValue, err = module.coerceAssignableValue(keyValue, keyType)
		if err != nil {
			return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
		}
		encoded, err := module.mapKey(keyValue)
		if err != nil {
			return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
		}
		data.deleteEntry(encoded)
	} else if _, err := setIndexValue(module, targetMap, keyValue, elemValue); err != nil {
		return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
	}
	if ctx.vm != nil {
		if err := ctx.vm.validateRuntimeValue(targetMap); err != nil {
			return nil, err
		}
	}
	if hasTarget {
		if err := storePointer(targetPtr, targetMap); err != nil {
			return []vmValue{newVMValue("String", err.Error()), newVMValue("Bool", false)}, nil
		}
	}
	return []vmValue{newVMValue("String", ""), newVMValue("Bool", true)}, nil
}

func reflectValueOK(value vmValue) []vmValue {
	return []vmValue{value, newVMValue("String", ""), newVMValue("Bool", true)}
}

func reflectValueError(message string) []vmValue {
	if strings.TrimSpace(message) == "" {
		message = "reflect: invalid Value"
	}
	return []vmValue{zeroReflectValueValue(), newVMValue("String", message), newVMValue("Bool", false)}
}
