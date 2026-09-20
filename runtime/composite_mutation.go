package runtime

import (
	"errors"
	"fmt"

	artifact "github.com/d7z-team/mini-go/runtime/bytecode"
)

func setIndexValue(module *moduleInstance, object, index, value vmValue) (vmValue, error) {
	return setIndexValueMode(module, object, index, value, true)
}

func setIndexValueMode(module *moduleInstance, object, index, value vmValue, clone bool) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		target, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		updated, err := setIndexValueMode(module, target, index, value, clone)
		if err != nil {
			return vmValue{}, err
		}
		return object, commitPointerMutation(object, updated)
	}
	switch data := object.Data.(type) {
	case *vmSlice:
		i, err := asInt64(index)
		if err != nil {
			return vmValue{}, err
		}
		if data == nil || i < 0 || int(i) >= data.Len {
			return vmValue{}, newGuestPanic(fmt.Errorf("slice index out of range: %d", i))
		}
		elemType := module.arrayElemType(object.Type)
		normalized, err := module.coerceAssignableValue(value, elemType)
		if err != nil {
			return vmValue{}, fmt.Errorf("slice element: %w", err)
		}
		if clone {
			normalized = module.cloneValueForStore(normalized)
			normalized = module.assignPreparedValue(data.valueAt(int(i)), normalized)
		}
		return object, data.setValueAt(int(i), normalized)
	case *vmArray:
		i, err := asInt64(index)
		if err != nil {
			return vmValue{}, err
		}
		if i < 0 || int(i) >= data.Len {
			return vmValue{}, newGuestPanic(fmt.Errorf("array index out of range: %d", i))
		}
		elemType := module.arrayElemType(object.Type)
		normalized, err := module.coerceAssignableValue(value, elemType)
		if err != nil {
			return vmValue{}, fmt.Errorf("array element: %w", err)
		}
		if clone {
			normalized = module.cloneValueForStore(normalized)
			normalized = module.assignPreparedValue(data.valueAt(int(i)), normalized)
		}
		return object, data.setValueAt(int(i), normalized)
	case *vmMap:
		if data == nil {
			return vmValue{}, newGuestPanic(errors.New("assignment to entry in nil map"))
		}
		keyValue := index
		valueToStore := value
		if keyType, valueType, ok := module.mapKeyValueTypes(object.Type); ok {
			normalizedKey, err := module.coerceAssignableValue(index, keyType)
			if err != nil {
				return vmValue{}, fmt.Errorf("map key: %w", err)
			}
			keyValue = normalizedKey
			normalizedValue, err := module.coerceAssignableValue(value, valueType)
			if err != nil {
				return vmValue{}, fmt.Errorf("map value: %w", err)
			}
			valueToStore = normalizedValue
		}
		key, err := module.mapKey(keyValue)
		if err != nil {
			return vmValue{}, err
		}
		key = data.keyForStore(key)
		limit := 0
		if module.vm != nil {
			limit = module.vm.limits.MaxCollectionElements
		}
		if _, exists := data.loadEntry(key); !exists && module.vm != nil {
			count := int64(data.length()) + 1
			if _, _, err := module.vm.checkCollectionSize(count, count); err != nil {
				return vmValue{}, err
			}
			if err := module.vm.chargeAllocationBytes(artifact.RuntimeMapEntryBytes); err != nil {
				return vmValue{}, err
			}
		}
		if err := data.storeWithinLimit(key, vmMapEntry{
			Key:   module.cloneValueForStore(keyValue),
			Value: module.cloneValueForStore(valueToStore),
		}, limit); err != nil {
			return vmValue{}, err
		}
		return object, nil
	default:
		if module.isMapType(object.Type) && object.Data == nil {
			return vmValue{}, newGuestPanic(errors.New("assignment to entry in nil map"))
		}
		return vmValue{}, fmt.Errorf("cannot set index on %s", object.Type)
	}
}

func loadFieldValue(module *moduleInstance, object vmValue, field string) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		value, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		return loadFieldValue(module, value, field)
	}
	if !isStructValue(object.Data) {
		return vmValue{}, fmt.Errorf("cannot read member %q from %s", field, object.Type)
	}
	value, ok := structValueField(object.Data, field)
	if !ok {
		if fieldInfo, ok := module.structValueFieldInfo(object, field); ok {
			storage := object.Data.(*vmStruct)
			index, _, _ := storage.schema.field(field)
			return storage.initializeField(index, module.zeroValue(fieldInfo.RuntimeType)), nil
		}
		return vmValue{}, fmt.Errorf("missing field %q in %s", field, object.Type)
	}
	if fieldInfo, ok := module.structValueFieldInfo(object, field); ok && !value.Type.Equal(fieldInfo.RuntimeType) {
		return module.convertValue(value, fieldInfo.RuntimeType)
	}
	return value, nil
}

func storeFieldValue(module *moduleInstance, object vmValue, field string, value vmValue) (vmValue, error) {
	return storeFieldValueMode(module, object, field, value, true)
}

func storeFieldValueMode(module *moduleInstance, object vmValue, field string, value vmValue, clone bool) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		target, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		updated, err := storeFieldValueMode(module, target, field, value, clone)
		if err != nil {
			return vmValue{}, err
		}
		return object, commitPointerMutation(object, updated)
	}
	if !isStructValue(object.Data) {
		return vmValue{}, fmt.Errorf("cannot set member %q on %s", field, object.Type)
	}
	fieldInfo, schemaKnown := module.structValueFieldInfo(object, field)
	if _, exists := structValueField(object.Data, field); !exists && !schemaKnown {
		return vmValue{}, fmt.Errorf("missing field %q", field)
	}
	stored := value
	if schemaKnown {
		normalized, err := module.coerceAssignableRuntimeValue(value, fieldInfo.RuntimeType, fieldInfo.Variadic)
		if err != nil {
			return vmValue{}, fmt.Errorf("field %q: %w", field, err)
		}
		stored = normalized
	}
	if clone {
		stored = module.cloneValueForStore(stored)
		if current, ok := structValueField(object.Data, field); ok {
			stored = module.assignPreparedValue(current, stored)
		}
	}
	object.Data, _ = updatedStructValue(object.Data, field, stored)
	return object, nil
}
