package runtime

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/d7z-team/mini-go/compiler/types"
)

func makeSliceValue(module *moduleInstance, typ any, length, capacity vmValue) (vmValue, error) {
	runtimeType := module.resolvedRuntimeType(typ)
	typeText := runtimeType.String()
	if _, _, ok := module.arrayType(runtimeType); ok {
		return vmValue{}, fmt.Errorf("cannot make fixed array %s", typeText)
	}
	n, err := asInt64(length)
	if err != nil {
		return vmValue{}, err
	}
	if n < 0 {
		return vmValue{}, newGuestPanic(fmt.Errorf("slice length must be non-negative, got %d", n))
	}
	capacityValue := n
	if capacity.Type.Valid() {
		capacityValue, err = asInt64(capacity)
		if err != nil {
			return vmValue{}, err
		}
		if capacityValue < n {
			return vmValue{}, newGuestPanic(fmt.Errorf("slice capacity %d is smaller than length %d", capacityValue, n))
		}
	}
	lengthValue, capacityInt, err := module.vm.checkCollectionSize(n, capacityValue)
	if err != nil {
		return vmValue{}, err
	}
	if module.vm != nil {
		if err := module.vm.chargeRuntimeObject(capacityInt, 0); err != nil {
			return vmValue{}, err
		}
	}
	elemType := module.arrayElemType(typeText)
	if module.sameRuntimeType(elemType, "Uint8") {
		return newByteSliceHeaderValue(runtimeType, make([]byte, capacityInt), lengthValue, capacityInt), nil
	}
	backing := make([]vmValue, capacityInt)
	for i := range backing {
		if module != nil {
			backing[i] = module.zeroValue(elemType)
			continue
		}
		backing[i] = zeroVMValue(elemType)
	}
	return newSliceHeaderValue(runtimeType, backing, 0, lengthValue, capacityInt), nil
}

func indexValue(module *moduleInstance, object, index vmValue) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		target, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		return indexValue(module, target, index)
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
		value := data.valueAt(int(i))
		return module.projectElementValue(value, object.Type)
	case *vmArray:
		if module.isSliceType(object.Type) {
			return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
		}
		i, err := asInt64(index)
		if err != nil {
			return vmValue{}, err
		}
		if i < 0 || int(i) >= data.Len {
			return vmValue{}, newGuestPanic(fmt.Errorf("array index out of range: %d", i))
		}
		return module.projectElementValue(data.valueAt(int(i)), object.Type)
	case *vmMap:
		key, err := module.normalizedMapKey(object, index)
		if err != nil {
			return vmValue{}, err
		}
		if data != nil {
			if entry, ok := data.loadEntry(key); ok {
				return module.projectElementValue(entry.Value, object.Type)
			}
		}
		if _, valueType, ok := module.mapKeyValueTypes(object.Type); ok {
			if module != nil {
				return module.zeroValue(valueType), nil
			}
			return zeroVMValue(valueType), nil
		}
		return vmValue{}, fmt.Errorf("missing map key %s", key)
	case string:
		i, err := asInt64(index)
		if err != nil {
			return vmValue{}, err
		}
		if i < 0 || int(i) >= len(data) {
			return vmValue{}, newGuestPanic(fmt.Errorf("string index out of range: %d", i))
		}
		return newVMValue("Uint8", uint64(data[i])), nil
	default:
		if module.isMapType(object.Type) && object.Data == nil {
			if _, err := module.normalizedMapKey(object, index); err != nil {
				return vmValue{}, err
			}
			if _, valueType, ok := module.mapKeyValueTypes(object.Type); ok {
				if module != nil {
					return module.zeroValue(valueType), nil
				}
				return zeroVMValue(valueType), nil
			}
		}
		return vmValue{}, fmt.Errorf("cannot index %s", object.Type)
	}
}

// Converted containers retain their backing storage but expose the element type
// declared by the view, including anonymous structs with different field tags.
func (m *moduleInstance) projectElementValue(value vmValue, container vmType) (vmValue, error) {
	if value.Type.Ref.Kind == types.Primitive || value.Type.Ref.Kind == types.Any {
		return value, nil
	}
	target := m.arrayElemType(container)
	if m.isMapType(container) {
		_, target, _ = m.mapKeyValueTypes(container)
	}
	if value.Type.String() == target || target == "" {
		return value, nil
	}
	return m.convertValue(value, target)
}

func mapIndexOKValues(module *moduleInstance, object, index vmValue) (vmValue, vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		target, err := derefPointer(object)
		if err != nil {
			return vmValue{}, vmValue{}, err
		}
		return mapIndexOKValues(module, target, index)
	}
	key, err := module.normalizedMapKey(object, index)
	if err != nil {
		return vmValue{}, vmValue{}, err
	}
	switch data := object.Data.(type) {
	case *vmMap:
		if data != nil {
			if entry, exists := data.loadEntry(key); exists {
				value, err := module.projectElementValue(entry.Value, object.Type)
				return value, newVMValue("Bool", true), err
			}
		}
	case nil:
		if !module.isMapType(object.Type) {
			return vmValue{}, vmValue{}, fmt.Errorf("cannot map-index-ok %s", object.Type)
		}
	default:
		return vmValue{}, vmValue{}, fmt.Errorf("cannot map-index-ok %s", object.Type)
	}
	_, valueType, typed := module.mapKeyValueTypes(object.Type)
	if !typed {
		return vmValue{}, vmValue{}, fmt.Errorf("missing map key %s", key)
	}
	if module != nil {
		return module.zeroValue(valueType), newVMValue("Bool", false), nil
	}
	return zeroVMValue(valueType), newVMValue("Bool", false), nil
}

func stringRuneAtValue(object, index vmValue) (vmValue, error) {
	data, ok := object.Data.(string)
	if !ok {
		return vmValue{}, fmt.Errorf("cannot read rune from %s", object.Type)
	}
	i, err := asInt64(index)
	if err != nil {
		return vmValue{}, err
	}
	if i < 0 || int(i) >= len(data) {
		return vmValue{}, fmt.Errorf("string index out of range: %d", i)
	}
	r, _ := utf8.DecodeRuneInString(data[int(i):])
	return newVMValue("Int32", int64(r)), nil
}

func stringNextIndexValue(object, index vmValue) (vmValue, error) {
	data, ok := object.Data.(string)
	if !ok {
		return vmValue{}, fmt.Errorf("cannot advance string index for %s", object.Type)
	}
	i, err := asInt64(index)
	if err != nil {
		return vmValue{}, err
	}
	if i < 0 || int(i) > len(data) {
		return vmValue{}, fmt.Errorf("string index out of range: %d", i)
	}
	if int(i) == len(data) {
		return newVMValue("Int", i), nil
	}
	_, size := utf8.DecodeRuneInString(data[int(i):])
	if size <= 0 {
		size = 1
	}
	return newVMValue("Int", i+int64(size)), nil
}

func sliceValue(module *moduleInstance, object, start, end, maxValue vmValue) (vmValue, error) {
	if _, ok := object.Type.PointerElem(); ok {
		target, err := derefPointer(object)
		if err != nil {
			return vmValue{}, err
		}
		if target.Type.ShapeKind() == types.Array {
			if err := commitPointerMutation(object, target); err != nil {
				return vmValue{}, err
			}
			target, err = derefPointer(object)
			if err != nil {
				return vmValue{}, err
			}
		}
		return sliceValue(module, target, start, end, maxValue)
	}
	lo, err := asInt64(start)
	if err != nil {
		return vmValue{}, err
	}
	hi, err := asInt64(end)
	if err != nil {
		return vmValue{}, err
	}
	if lo < 0 || hi < lo {
		return vmValue{}, newGuestPanic(fmt.Errorf("invalid slice bounds [%d:%d]", lo, hi))
	}
	full := maxValue.Type.Valid() && maxValue.Type.Ref.Kind != types.Void
	maxIndex := hi
	if full {
		maxIndex, err = asInt64(maxValue)
		if err != nil {
			return vmValue{}, err
		}
		if maxIndex < hi {
			return vmValue{}, newGuestPanic(fmt.Errorf("invalid full slice bounds [%d:%d:%d]", lo, hi, maxIndex))
		}
	}
	switch data := object.Data.(type) {
	case *vmSlice:
		bound := hi
		if full {
			bound = maxIndex
		}
		length := 0
		capacity := 0
		if data != nil {
			length = data.Len
			capacity = data.Cap
		}
		if hi > int64(capacity) || bound > int64(capacity) {
			return vmValue{}, newGuestPanic(fmt.Errorf("slice bounds out of range [%d:%d] with length %d capacity %d", lo, hi, length, capacity))
		}
		start := 0
		if data != nil {
			start = data.Start
		}
		resultCapacity := capacity - int(lo)
		if full {
			resultCapacity = int(bound - lo)
		}
		return newSliceViewValue(module.sliceResultType(object.Type), data, start+int(lo), int(hi-lo), resultCapacity), nil
	case *vmArray:
		array := data
		if module.isSliceType(object.Type) {
			return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
		}
		bound := hi
		if full {
			bound = maxIndex
		}
		if int(bound) > array.Len {
			return vmValue{}, newGuestPanic(fmt.Errorf("slice bounds out of range [%d:%d] with length %d", lo, hi, array.Len))
		}
		capacity := array.Len - int(lo)
		if full {
			capacity = int(bound - lo)
		}
		return newSliceViewValue(module.sliceResultType(object.Type), &array.vmSlice, array.Start+int(lo), int(hi-lo), capacity), nil
	case string:
		if full {
			return vmValue{}, errors.New("full slice is not supported for strings")
		}
		if int(hi) > len(data) {
			return vmValue{}, newGuestPanic(fmt.Errorf("slice bounds out of range [%d:%d] with length %d", lo, hi, len(data)))
		}
		text := data[int(lo):int(hi)]
		if module.vm != nil {
			if err := module.vm.chargeAllocationBytes(int64(len(text))); err != nil {
				return vmValue{}, err
			}
		}
		return newVMValue("String", text), nil
	default:
		return vmValue{}, fmt.Errorf("cannot slice %s", object.Type)
	}
}

func lenValue(module *moduleInstance, object vmValue) (vmValue, error) {
	if elem, ok := object.Type.PointerElem(); ok {
		if length, _, ok := module.arrayType(elem); ok {
			return newVMValue("Int", length), nil
		}
	}
	switch data := object.Data.(type) {
	case *vmSlice:
		if !module.isSliceType(object.Type) {
			return vmValue{}, fmt.Errorf("invalid array backing for %s", object.Type)
		}
		if data == nil {
			return newVMValue("Int", int64(0)), nil
		}
		return newVMValue("Int", int64(data.Len)), nil
	case *vmArray:
		if module.isSliceType(object.Type) {
			return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
		}
		return newVMValue("Int", int64(data.Len)), nil
	case *vmMap:
		if data == nil {
			return newVMValue("Int", int64(0)), nil
		}
		return newVMValue("Int", int64(data.length())), nil
	case string:
		return newVMValue("Int", int64(len(data))), nil
	case *waitableResource:
		return waitableLenValue(data), nil
	default:
		if module.isMapType(object.Type) && object.Data == nil {
			return newVMValue("Int", int64(0)), nil
		}
		if module.isWaitableTypeName(object.Type) && object.Data == nil {
			return waitableLenValue(nil), nil
		}
		return vmValue{}, fmt.Errorf("cannot len %s", object.Type)
	}
}

func capValue(module *moduleInstance, object vmValue) (vmValue, error) {
	if elem, ok := object.Type.PointerElem(); ok {
		if length, _, ok := module.arrayType(elem); ok {
			return newVMValue("Int", length), nil
		}
	}
	switch data := object.Data.(type) {
	case *vmSlice:
		if !module.isSliceType(object.Type) {
			return vmValue{}, fmt.Errorf("invalid array backing for %s", object.Type)
		}
		if data == nil {
			return newVMValue("Int", int64(0)), nil
		}
		return newVMValue("Int", int64(data.Cap)), nil
	case *vmArray:
		if module.isSliceType(object.Type) {
			return vmValue{}, fmt.Errorf("invalid slice backing for %s", object.Type)
		}
		return newVMValue("Int", int64(data.Len)), nil
	case *waitableResource:
		return waitableCapValue(data), nil
	default:
		if module.isWaitableTypeName(object.Type) && object.Data == nil {
			return waitableCapValue(nil), nil
		}
		return vmValue{}, fmt.Errorf("cannot cap %s", object.Type)
	}
}
