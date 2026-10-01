package runtime

import (
	"errors"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func appendValue(module *moduleInstance, object vmValue, values []vmValue, expand bool) (vmValue, error) {
	if module.isSliceType(object.Type) && module.sameRuntimeType(module.sequenceElementType(object.Type), "Uint8") {
		var bytes []byte
		expandedBytes := false
		if expand {
			if len(values) != 1 {
				return vmValue{}, errors.New("expanded append requires exactly one source argument")
			}
			if _, _, ok := module.arrayType(values[0].Type); ok {
				return vmValue{}, fmt.Errorf("expanded append source must be slice, got array %s", values[0].Type)
			}
			if text, ok := values[0].Data.(string); ok {
				bytes, expandedBytes = []byte(text), true
			} else if slice, ok := values[0].Data.(*vmSlice); ok && slice != nil && slice.ByteBacked {
				bytes, expandedBytes = slice.bytes(), true
			} else {
				expanded, ok := module.sliceValues(values[0])
				if !ok {
					return vmValue{}, fmt.Errorf("cannot expand append argument %s", values[0].Type)
				}
				values = expanded
			}
		}
		if !expandedBytes {
			bytes = make([]byte, len(values))
			for index, value := range values {
				normalized, err := module.coerceAssignableValue(value, "Uint8")
				if err != nil {
					return vmValue{}, err
				}
				n, err := numericAsUint64(normalized)
				if err != nil {
					return vmValue{}, err
				}
				bytes[index] = byte(n)
			}
		}
		return appendByteValues(module, object, bytes)
	}
	if _, _, ok := module.arrayType(object.Type); ok {
		return vmValue{}, fmt.Errorf("cannot append to array %s", object.Type)
	}
	if expand {
		if len(values) != 1 {
			return vmValue{}, errors.New("expanded append requires exactly one source argument")
		}
		if _, _, ok := module.arrayType(values[0].Type); ok {
			return vmValue{}, fmt.Errorf("expanded append source must be slice, got array %s", values[0].Type)
		}
		if _, ok := values[0].Data.(string); ok {
			return vmValue{}, fmt.Errorf("expanded string append requires a byte slice, got %s", object.Type)
		}
		expanded, ok := module.sliceValues(values[0])
		if !ok {
			return vmValue{}, fmt.Errorf("cannot expand append argument %s", values[0].Type)
		}
		values = expanded
	}
	if !module.isSliceType(object.Type) {
		return vmValue{}, fmt.Errorf("cannot append to %s", object.Type)
	}
	elemType := module.sequenceElementType(object.Type)
	normalizedValues := make([]vmValue, len(values))
	for i, value := range values {
		normalized, err := module.coerceAssignableValue(value, elemType)
		if err != nil {
			return vmValue{}, fmt.Errorf("append element %d: %w", i, err)
		}
		normalizedValues[i] = module.cloneValueForStore(normalized)
	}
	source, valid := object.Data.(*vmSlice)
	if !valid && object.Data != nil {
		return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
	}
	oldLen, oldCap := 0, 0
	if source != nil {
		oldLen, oldCap = source.Len, source.Cap
	}
	newLen, newCap, err := module.vm.reserveSliceAppend(oldLen, oldCap, len(values))
	if err != nil {
		return vmValue{}, err
	}
	if source == nil && newLen == 0 {
		return newSliceHeaderValue(object.Type, nil, 0, 0, 0), nil
	}
	var result vmValue
	if source != nil && newLen <= oldCap {
		result = newSliceViewValue(object.Type, source, source.Start, newLen, newCap)
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
	for i, value := range normalizedValues {
		if err := destination.setValueAt(oldLen+i, value); err != nil {
			return vmValue{}, fmt.Errorf("append element %d: %w", i, err)
		}
	}
	return result, nil
}

func appendByteValues(module *moduleInstance, object vmValue, bytes []byte) (vmValue, error) {
	if !module.isSliceType(object.Type) || !module.sameRuntimeType(module.sequenceElementType(object.Type), "Uint8") {
		return vmValue{}, fmt.Errorf("expected byte slice, got %s", object.Type)
	}
	source, ok := object.Data.(*vmSlice)
	if !ok && object.Data != nil {
		return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
	}
	oldLen, oldCap := 0, 0
	if source != nil {
		oldLen, oldCap = source.Len, source.Cap
	}
	newLen, newCap, err := module.vm.reserveSliceAppend(oldLen, oldCap, len(bytes))
	if err != nil {
		return vmValue{}, err
	}
	if source == nil && newLen == 0 {
		return newSliceHeaderValue(object.Type, nil, 0, 0, 0), nil
	}
	var result vmValue
	if source != nil && newLen <= oldCap {
		result = newSliceViewValue(object.Type, source, source.Start, newLen, newCap)
	} else {
		backing := make([]byte, newCap)
		if source != nil {
			if source.ByteBacked {
				copy(backing, source.bytes())
			} else {
				for i, value := range source.values() {
					n, err := numericAsUint64(value)
					if err != nil {
						return vmValue{}, err
					}
					backing[i] = byte(n)
				}
			}
		}
		result = newByteSliceHeaderValue(object.Type, backing, newLen, newCap)
	}
	result.Data.(*vmSlice).writeBytes(oldLen, bytes)
	return result, nil
}

// reserveSliceAppend validates growth and charges the added logical slots before
// either the byte or value backing is changed.
func (vm *vm) reserveSliceAppend(length, capacity, added int) (int, int, error) {
	newLength, newCapacity := int64(length)+int64(added), int64(capacity)
	if newLength > newCapacity {
		newCapacity = max(newLength, int64(capacity)*2)
	}
	newLen, newCap, err := vm.checkCollectionSize(newLength, newCapacity)
	if err != nil {
		return 0, 0, err
	}
	if newCap > capacity {
		if err := vm.chargeAllocationBytes(int64(newCap-capacity) * ir.RuntimeSlotBytes); err != nil {
			return 0, 0, err
		}
	}
	return newLen, newCap, nil
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
		elemType := module.sequenceElementType(object.Type)
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
		elemType := module.sequenceElementType(object.Type)
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
	elemType := module.sequenceElementType(dst.Type)
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
		if !module.sameRuntimeType(module.sequenceElementType(src.Type), elemType) {
			return vmValue{}, fmt.Errorf("copy element 0: %s is not %s", module.sequenceElementType(src.Type), elemType)
		}
		srcLen := 0
		if data != nil {
			srcLen = data.Len
		}
		count := min(dstLen, srcLen)
		if count > 0 && dstSlice.ByteBacked && data.ByteBacked {
			dstSlice.copyBytesFrom(data, count)
			return newVMValue("Int", int64(count)), nil
		}
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
		if dstSlice != nil && dstSlice.ByteBacked && module.sameRuntimeType(elemType, "Uint8") {
			dstSlice.writeBytes(0, []byte(data[:count]))
			return newVMValue("Int", int64(count)), nil
		}
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
