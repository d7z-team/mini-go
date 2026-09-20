package runtime

import (
	"errors"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func appendValue(module *moduleInstance, object vmValue, values []vmValue, expand bool) (vmValue, error) {
	if _, _, ok := module.arrayType(object.Type); ok {
		return vmValue{}, fmt.Errorf("cannot append to array %s", object.Type)
	}
	var expandedString string
	stringExpansion := false
	if expand {
		if len(values) != 1 {
			return vmValue{}, errors.New("expanded append requires exactly one source argument")
		}
		if _, _, ok := module.arrayType(values[0].Type); ok {
			return vmValue{}, fmt.Errorf("expanded append source must be slice, got array %s", values[0].Type)
		}
		if text, ok := values[0].Data.(string); ok {
			if !module.sameRuntimeType(module.arrayElemType(object.Type), "Uint8") {
				return vmValue{}, fmt.Errorf("expanded string append requires a byte slice, got %s", object.Type)
			}
			expandedString = text
			stringExpansion = true
		} else {
			expanded, ok := module.sliceValues(values[0])
			if !ok {
				return vmValue{}, fmt.Errorf("cannot expand append argument %s", values[0].Type)
			}
			values = expanded
		}
	}
	if !module.isSliceType(object.Type) {
		return vmValue{}, fmt.Errorf("cannot append to %s", object.Type)
	}
	elemType := module.arrayElemType(object.Type)
	var normalizedValues []vmValue
	valueCount := len(expandedString)
	if !stringExpansion {
		valueCount = len(values)
		normalizedValues = make([]vmValue, len(values))
		for i, value := range values {
			normalized, err := module.coerceAssignableValue(value, elemType)
			if err != nil {
				return vmValue{}, fmt.Errorf("append element %d: %w", i, err)
			}
			normalizedValues[i] = module.cloneValueForStore(normalized)
		}
	}
	oldLen, _ := sliceLen(object)
	oldCap, _ := sliceCap(object)
	newLength := int64(oldLen) + int64(valueCount)
	newCapacity := int64(oldCap)
	if newLength > newCapacity {
		newCapacity = newLength
		if doubled := int64(oldCap) * 2; doubled > newCapacity {
			newCapacity = doubled
		}
	}
	newLen, newCap, err := module.vm.checkCollectionSize(newLength, newCapacity)
	if err != nil {
		return vmValue{}, err
	}
	if module.vm != nil && newCap > oldCap {
		if err := module.vm.chargeAllocationBytes(int64(newCap-oldCap) * ir.RuntimeSlotBytes); err != nil {
			return vmValue{}, err
		}
	}
	source, valid := object.Data.(*vmSlice)
	if !valid && object.Data != nil {
		return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
	}
	if source == nil && newLen == 0 {
		return newSliceHeaderValue(object.Type, nil, 0, 0, 0), nil
	}
	var result vmValue
	if source != nil && newLen <= oldCap {
		result = newSliceViewValue(object.Type, source, source.Start, newLen, newCap)
	} else if module.sameRuntimeType(elemType, "Uint8") {
		backing := make([]byte, newCap)
		if source != nil {
			if source.ByteBacked {
				copy(backing, source.bytes())
			} else {
				for i, value := range source.values() {
					n, err := numericAsUint64(value)
					if err != nil {
						return vmValue{}, fmt.Errorf("append existing byte %d: %w", i, err)
					}
					backing[i] = byte(n)
				}
			}
		}
		result = newByteSliceHeaderValue(object.Type, backing, newLen, newCap)
	} else {
		backing := make([]vmValue, newCap)
		if source != nil {
			copy(backing, source.values())
		}
		for i := oldLen; i < newCap; i++ {
			backing[i] = module.zeroValue(elemType)
		}
		result = newSliceHeaderValue(object.Type, backing, 0, newLen, newCap)
	}
	destination := result.Data.(*vmSlice)
	if stringExpansion {
		destination.writeBytes(oldLen, []byte(expandedString))
	} else {
		for i, value := range normalizedValues {
			if err := destination.setValueAt(oldLen+i, value); err != nil {
				return vmValue{}, fmt.Errorf("append element %d: %w", i, err)
			}
		}
	}
	return result, nil
}

func deleteValue(module *moduleInstance, object, key vmValue) error {
	mapKey, err := module.normalizedMapKey(object, key)
	if err != nil {
		return err
	}
	switch data := object.Data.(type) {
	case *vmMap:
		if data != nil {
			data.deleteEntry(mapKey)
		}
	case nil:
		if !module.isMapType(object.Type) {
			return fmt.Errorf("cannot delete from %s", object.Type)
		}
	default:
		return fmt.Errorf("cannot delete from %s", object.Type)
	}
	return nil
}

func mapKeysValue(module *moduleInstance, object vmValue) (vmValue, error) {
	keyType := "Any"
	if parsedKeyType, _, ok := module.mapKeyValueTypes(object.Type); ok {
		keyType = parsedKeyType
	}
	var values []vmValue
	switch data := object.Data.(type) {
	case *vmMap:
		entries := data.snapshot()
		keys := sortedVMMapKeys(entries)
		values = make([]vmValue, 0, len(keys))
		for _, key := range keys {
			values = append(values, module.cloneValueForStore(entries[key].Key))
		}
	case nil:
		if !module.isMapType(object.Type) {
			return vmValue{}, fmt.Errorf("cannot get map keys from %s", object.Type)
		}
	default:
		return vmValue{}, fmt.Errorf("cannot get map keys from %s", object.Type)
	}
	return newSliceValue("Slice<"+keyType+">", values), nil
}

func clearValue(module *moduleInstance, object vmValue) error {
	if _, _, ok := module.arrayType(object.Type); ok {
		return fmt.Errorf("cannot clear array %s", object.Type)
	}
	switch data := object.Data.(type) {
	case *vmSlice:
		if data == nil {
			return nil
		}
		elemType := module.arrayElemType(object.Type)
		for i := 0; i < data.Len; i++ {
			var zero vmValue
			if module != nil {
				zero = module.zeroValue(elemType)
			} else {
				zero = zeroVMValue(elemType)
			}
			zero = module.assignPreparedValue(data.valueAt(i), zero)
			if err := data.setValueAt(i, zero); err != nil {
				return err
			}
		}
		return nil
	case *vmArray:
		if module.isSliceType(object.Type) {
			return fmt.Errorf("invalid slice backing for %s", object.Type)
		}
		elemType := module.arrayElemType(object.Type)
		for i := range data.Len {
			var zero vmValue
			if module != nil {
				zero = module.zeroValue(elemType)
			} else {
				zero = zeroVMValue(elemType)
			}
			zero = module.assignPreparedValue(data.valueAt(i), zero)
			if err := data.setValueAt(i, zero); err != nil {
				return err
			}
		}
		return nil
	case *vmMap:
		if data != nil {
			data.clear()
		}
		return nil
	case nil:
		if module.isSliceType(object.Type) || module.isMapType(object.Type) {
			return nil
		}
		return fmt.Errorf("cannot clear %s", object.Type)
	default:
		return fmt.Errorf("cannot clear %s", object.Type)
	}
}

func copyValue(module *moduleInstance, dst, src vmValue) (vmValue, error) {
	if _, _, ok := module.arrayType(dst.Type); ok {
		return vmValue{}, fmt.Errorf("copy destination must be slice, got array %s", dst.Type)
	}
	if _, _, ok := module.arrayType(src.Type); ok {
		return vmValue{}, fmt.Errorf("copy source must be slice or string, got array %s", src.Type)
	}
	if !module.isSliceType(dst.Type) {
		return vmValue{}, fmt.Errorf("copy destination must be array or slice, got %s", dst.Type)
	}
	elemType := module.arrayElemType(dst.Type)
	dstSlice, ok := dst.Data.(*vmSlice)
	if !ok {
		return vmValue{}, fmt.Errorf("copy destination must be slice, got %s", dst.Type)
	}
	dstLen := 0
	if dstSlice != nil {
		dstLen = dstSlice.Len
	}
	switch data := src.Data.(type) {
	case *vmSlice:
		if !module.sameRuntimeType(module.arrayElemType(src.Type), elemType) {
			return vmValue{}, fmt.Errorf("copy element 0: %s is not %s", module.arrayElemType(src.Type), elemType)
		}
		srcLen := 0
		if data != nil {
			srcLen = data.Len
		}
		count := min(dstLen, srcLen)
		copied := make([]vmValue, count)
		for i := 0; i < count; i++ {
			normalized, err := module.coerceAssignableValue(data.valueAt(i), elemType)
			if err != nil {
				return vmValue{}, fmt.Errorf("copy element %d: %w", i, err)
			}
			copied[i] = module.cloneValueForStore(normalized)
		}
		for i := range copied {
			value := module.assignPreparedValue(dstSlice.valueAt(i), copied[i])
			if err := dstSlice.setValueAt(i, value); err != nil {
				return vmValue{}, fmt.Errorf("copy element %d: %w", i, err)
			}
		}
		return newVMValue("Int", int64(count)), nil
	case string:
		count := min(dstLen, len(data))
		for i := 0; i < count; i++ {
			normalized, err := module.coerceAssignableValue(newVMValue("Uint8", uint64(data[i])), elemType)
			if err != nil {
				return vmValue{}, fmt.Errorf("copy byte %d: %w", i, err)
			}
			if err := dstSlice.setValueAt(i, normalized); err != nil {
				return vmValue{}, fmt.Errorf("copy byte %d: %w", i, err)
			}
		}
		return newVMValue("Int", int64(count)), nil
	case nil:
		return newVMValue("Int", int64(0)), nil
	default:
		return vmValue{}, fmt.Errorf("copy source must be array, slice, or string, got %s", src.Type)
	}
}
