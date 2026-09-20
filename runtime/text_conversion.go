package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (m *moduleInstance) isByteSliceType(typ any) bool {
	elem, ok := m.resolvedRuntimeType(typ).SliceElem()
	return ok && elem.Primitive(types.PrimitiveUint8)
}

func (m *moduleInstance) isRuneSliceType(typ any) bool {
	elem, ok := m.resolvedRuntimeType(typ).SliceElem()
	return ok && elem.Primitive(types.PrimitiveInt32)
}

func convertStringToByteSlice(value vmValue, target string) (vmValue, error) {
	if !value.Type.Primitive(types.PrimitiveString) {
		return vmValue{}, fmt.Errorf("cannot convert %s to %s", value.Type, target)
	}
	text, ok := value.Data.(string)
	if !ok {
		return vmValue{}, errors.New("invalid String value")
	}
	return newByteSliceValue(target, text), nil
}

func convertStringToRuneSlice(value vmValue, target string) (vmValue, error) {
	if !value.Type.Primitive(types.PrimitiveString) {
		return vmValue{}, fmt.Errorf("cannot convert %s to %s", value.Type, target)
	}
	text, ok := value.Data.(string)
	if !ok {
		return vmValue{}, errors.New("invalid String value")
	}
	out := make([]vmValue, 0, len(text))
	for _, r := range text {
		out = append(out, newVMValue("Int32", int64(r)))
	}
	return newOwnedSliceValue(target, out), nil
}

func (m *moduleInstance) convertByteSliceToString(value vmValue) (vmValue, bool, error) {
	if !m.isByteSliceType(value.Type) {
		return vmValue{}, false, nil
	}
	slice, ok := value.Data.(*vmSlice)
	if !ok {
		return vmValue{}, true, fmt.Errorf("invalid %s value", value.Type)
	}
	if slice != nil && slice.ByteBacked {
		text := string(slice.bytes())
		if m.vm != nil {
			if err := m.vm.chargeAllocationBytes(int64(len(text))); err != nil {
				return vmValue{}, true, err
			}
		}
		return newVMValue("String", text), true, nil
	}
	items, _ := sliceValues(value)
	out := make([]byte, len(items))
	for i, item := range items {
		item, err := m.coerceAssignableValue(item, "Uint8")
		if err != nil {
			return vmValue{}, true, fmt.Errorf("byte slice element %d: %w", i, err)
		}
		n, err := numericAsUint64(item)
		if err != nil {
			return vmValue{}, true, fmt.Errorf("byte slice element %d: %w", i, err)
		}
		out[i] = byte(n)
	}
	if m.vm != nil {
		if err := m.vm.chargeAllocationBytes(int64(len(out))); err != nil {
			return vmValue{}, true, err
		}
	}
	return newVMValue("String", string(out)), true, nil
}

func (m *moduleInstance) convertRuneSliceToString(value vmValue) (vmValue, bool, error) {
	if !m.isRuneSliceType(value.Type) {
		return vmValue{}, false, nil
	}
	items, ok := sliceValues(value)
	if !ok {
		return vmValue{}, true, fmt.Errorf("invalid %s value", value.Type)
	}
	out := make([]rune, len(items))
	for i, item := range items {
		item, err := m.coerceAssignableValue(item, "Int32")
		if err != nil {
			return vmValue{}, true, fmt.Errorf("rune slice element %d: %w", i, err)
		}
		n, err := numericAsInt64(item)
		if err != nil {
			return vmValue{}, true, fmt.Errorf("rune slice element %d: %w", i, err)
		}
		out[i] = rune(n)
	}
	text := string(out)
	if m.vm != nil {
		if err := m.vm.chargeAllocationBytes(int64(len(text))); err != nil {
			return vmValue{}, true, err
		}
	}
	return newVMValue("String", text), true, nil
}
