package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

func setIndexValue(module *moduleInstance, object, index, value vmValue) (vmValue, error) {
	return setIndexValueModeWithBatch(module, object, index, value, true, nil)
}

func setIndexValueMode(module *moduleInstance, object, index, value vmValue, clone bool) (vmValue, error) {
	return setIndexValueModeWithBatch(module, object, index, value, clone, nil)
}

func setIndexValueWithBatch(module *moduleInstance, object, index, value vmValue, batch *managedAllocationBatch) (vmValue, error) {
	return setIndexValueModeWithBatch(module, object, index, value, true, batch)
}

func setIndexValueModeWithBatch(module *moduleInstance, object, index, value vmValue, clone bool, batch *managedAllocationBatch) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		target, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		updated, err := setIndexValueModeWithBatch(module, target, index, value, clone, batch)
		if err != nil {
			return vmValue{}, err
		}
		return object, commitPointerMutation(object, updated)
	}
	switch data := object.payload().(type) {
	case *vmSlice:
		i, err := asInt64(index)
		if err != nil {
			return vmValue{}, err
		}
		if data == nil || i < 0 || int(i) >= data.Len {
			return vmValue{}, newGuestPanic(fmt.Errorf("slice index out of range: %d", i))
		}
		elemType := module.sequenceElementType(object.Type)
		normalized, err := module.coerceAssignableValue(value, elemType)
		if err != nil {
			return vmValue{}, fmt.Errorf("slice element: %w", err)
		}
		if clone && data.managedHeap == nil && data.inlineRaw == nil {
			normalized = module.cloneValueForStore(normalized)
			normalized = module.assignPreparedValue(data.valueAt(int(i)), normalized)
		}
		if data.managedHeap == nil && data.inlineRaw == nil {
			normalized = module.adoptChild(normalized)
		}
		if err := data.setValueAt(int(i), normalized); err != nil {
			return vmValue{}, err
		}
		object.setPayload(data)
		return object, nil
	case *vmArray:
		i, err := asInt64(index)
		if err != nil {
			return vmValue{}, err
		}
		if i < 0 || int(i) >= data.Len {
			return vmValue{}, newGuestPanic(fmt.Errorf("array index out of range: %d", i))
		}
		elemType := module.sequenceElementType(object.Type)
		normalized, err := module.coerceAssignableValue(value, elemType)
		if err != nil {
			return vmValue{}, fmt.Errorf("array element: %w", err)
		}
		if clone && data.managedHeap == nil && data.inlineRaw == nil {
			normalized = module.cloneValueForStore(normalized)
			normalized = module.assignPreparedValue(data.valueAt(int(i)), normalized)
		}
		if data.managedHeap == nil && data.inlineRaw == nil {
			normalized = module.adoptChild(normalized)
		}
		if err := data.setValueAt(int(i), normalized); err != nil {
			return vmValue{}, err
		}
		object.setPayload(data)
		return object, nil
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
		mutationBatch := batch
		var ownedBatch *managedAllocationBatch
		if mutationBatch == nil && data.managed != nil &&
			managedMapValueNeedsAllocation(keyValue, valueToStore) {
			var err error
			ownedBatch, err = data.managed.acquireMutationBatch()
			if err != nil {
				return vmValue{}, err
			}
			mutationBatch = ownedBatch
		}
		result, mutationErr := func() (vmValue, error) {
			valueToStore = module.adoptValueWithBatch(valueToStore, mutationBatch)
			if mutationBatch != nil && mutationBatch.failure != nil {
				return vmValue{}, mutationBatch.failure
			}
			keyValue = module.adoptValueWithBatch(keyValue, mutationBatch)
			if mutationBatch != nil && mutationBatch.failure != nil {
				return vmValue{}, mutationBatch.failure
			}
			key, err := module.mapKey(keyValue)
			if err != nil {
				return vmValue{}, err
			}
			key = data.keyForStore(key)
			if data.managed == nil {
				keyValue = module.cloneValueForStore(keyValue)
				valueToStore = module.cloneValueForStore(valueToStore)
			}
			if err := data.storeWithBatch(key, vmMapEntry{
				Key:   keyValue,
				Value: valueToStore,
			}, mutationBatch); err != nil {
				return vmValue{}, err
			}
			return object, nil
		}()
		if ownedBatch != nil {
			if mutationErr != nil {
				_ = rollbackMutationBatch(ownedBatch)
				return vmValue{}, mutationErr
			}
			if err := ownedBatch.flush(); err != nil {
				_ = rollbackMutationBatch(ownedBatch)
				return vmValue{}, err
			}
		}
		return result, mutationErr
	default:
		if module.isMapType(object.Type) && object.payload() == nil {
			return vmValue{}, newGuestPanic(errors.New("assignment to entry in nil map"))
		}
		return vmValue{}, fmt.Errorf("cannot set index on %s", object.Type)
	}
}

func managedMapValueNeedsAllocation(values ...vmValue) bool {
	for _, value := range values {
		aggregateZero := value.Data == nil &&
			(value.Type.ShapeKind() == types.Array || value.Type.ShapeKind() == types.Struct)
		if value.scalarKind == 0 && !value.Type.Primitive(types.PrimitiveBool) &&
			!value.ref.valid() && !(value.Data == nil && !aggregateZero) {
			return true
		}
	}
	return false
}

func loadFieldValue(module *moduleInstance, object vmValue, field string) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		value, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		return loadFieldValue(module, value, field)
	}
	payload := object.payload()
	if !isStructValue(payload) {
		return vmValue{}, fmt.Errorf("cannot read member %q from %s", field, object.Type)
	}
	storage := payload.(*vmStruct)
	fieldInfo, schemaKnown := module.structFieldInfo(object.Type, storage, field)
	value, ok := structValueField(payload, field)
	if !ok {
		if schemaKnown {
			index, _, _ := storage.schema.field(field)
			return storage.initializeField(index, module.zeroValue(fieldInfo.RuntimeType)), nil
		}
		return vmValue{}, fmt.Errorf("missing field %q in %s", field, object.Type)
	}
	if schemaKnown && !value.Type.Equal(fieldInfo.RuntimeType) {
		converted, err := module.convertValue(value, fieldInfo.RuntimeType)
		if err != nil {
			return vmValue{}, err
		}
		value = converted
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
	payload := object.payload()
	if !isStructValue(payload) {
		return vmValue{}, fmt.Errorf("cannot set member %q on %s", field, object.Type)
	}
	storage := payload.(*vmStruct)
	fieldInfo, schemaKnown := module.structFieldInfo(object.Type, storage, field)
	if _, exists := structValueField(payload, field); !exists && !schemaKnown {
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
	if clone && storage.managedHeap == nil && storage.inlineRaw == nil {
		stored = module.cloneValueForStore(stored)
		if current, ok := structValueField(payload, field); ok {
			stored = module.assignPreparedValue(current, stored)
		}
	}
	if storage.managedHeap == nil && storage.inlineRaw == nil {
		stored = module.adoptChild(stored)
	}
	updated, _ := updatedStructValue(payload, field, stored)
	object.setPayload(updated)
	return object, nil
}
