package runtime

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type vmPointer struct {
	Type     vmType
	Identity string
	original *vmPointer
	target   pointerTarget
	module   *moduleInstance
	cell     *slot
	parent   vmValue
	field    string
	index    int64
	array    *vmSlice
	arrayLen int
	slot     *slot
	path     []ir.AddressPathSegment
	indexes  []vmValue
}

type pointerTarget uint8

const (
	pointerSlot pointerTarget = iota
	pointerCell
	pointerField
	pointerIndex
	pointerArray
)

func newTargetPointer(pointer *vmPointer) vmValue {
	if pointer.Identity == "" {
		pointer.Identity = fmt.Sprintf("pointer:%p", pointer)
	}
	return newVMValue(pointerType(pointer.Type), pointer)
}

func newSlotPointerValue(typ any, identity string, cell *slot) vmValue {
	pointerTypeValue := coerceRuntimeType(typ)
	return newVMValue(pointerType(pointerTypeValue), &vmPointer{Type: pointerTypeValue, Identity: identity, slot: cell})
}

func newPathPointerValue(typ any, identity string, cell *slot, path []ir.AddressPathSegment, indexes []vmValue) vmValue {
	pointerTypeValue := coerceRuntimeType(typ)
	return newVMValue(pointerType(pointerTypeValue), &vmPointer{
		Type: pointerTypeValue, Identity: identity, slot: cell,
		path: path, indexes: indexes,
	})
}

func (pointer *vmPointer) loadValue() (vmValue, error) {
	if pointer.original != nil {
		value, err := pointer.original.loadValue()
		value.Type = pointer.Type
		return value, err
	}
	switch pointer.target {
	case pointerCell:
		if pointer.cell == nil {
			return vmValue{}, errors.New("reflect: invalid cell pointer")
		}
		return pointer.cell.load(), nil
	case pointerField, pointerIndex:
		parent := pointer.parent
		if parent.Type.ShapeKind() == types.Pointer {
			var err error
			parent, err = derefPointer(parent)
			if err != nil {
				return vmValue{}, err
			}
		}
		if pointer.target == pointerField {
			return loadFieldValue(pointer.module, parent, pointer.field)
		}
		return indexValue(pointer.module, parent, newVMValue("Int", pointer.index))
	case pointerArray:
		return newVMValue(pointer.Type, &vmArray{vmSlice: vmSlice{
			vmSliceStorage: pointer.array.vmSliceStorage,
			Start:          pointer.array.Start, Len: pointer.arrayLen, Cap: pointer.arrayLen,
		}}), nil
	}
	if pointer.slot == nil {
		return vmValue{}, errors.New("invalid pointer")
	}
	if len(pointer.path) == 0 {
		return pointer.slot.load(), nil
	}
	return loadAddressPath(pointer.slot.module, pointer.slot.load(), pointer.path, pointer.indexes)
}

func (pointer *vmPointer) storeValue(value vmValue, mutation bool) error {
	if pointer.original != nil {
		value.Type = pointer.original.Type
		return pointer.original.storeValue(value, mutation)
	}
	switch pointer.target {
	case pointerCell:
		if pointer.cell == nil {
			return errors.New("reflect: invalid cell pointer")
		}
		normalized, err := pointer.module.coerceAssignableValue(value, pointer.Type)
		if err != nil {
			return err
		}
		if mutation {
			pointer.cell.publish(normalized)
		} else {
			return pointer.cell.store(normalized)
		}
		return nil
	case pointerField, pointerIndex:
		parent := pointer.parent
		indirect := parent.Type.ShapeKind() == types.Pointer
		var err error
		if indirect {
			parent, err = derefPointer(parent)
			if err != nil {
				return err
			}
		}
		if pointer.target == pointerField {
			parent, err = storeFieldValueMode(pointer.module, parent, pointer.field, value, !mutation)
		} else {
			parent, err = setIndexValueMode(pointer.module, parent, newVMValue("Int", pointer.index), value, !mutation)
		}
		if err != nil {
			return err
		}
		if indirect {
			return commitPointerMutation(pointer.parent, parent)
		}
		return nil
	case pointerArray:
		array, ok := value.Data.(*vmArray)
		if !ok || array.Len != pointer.arrayLen {
			return fmt.Errorf("array assignment requires length %d", pointer.arrayLen)
		}
		values := array.values()
		prepared := make([]vmValue, len(values))
		_, elem, _ := pointer.Type.ArrayInfo()
		for i, item := range values {
			normalized, err := pointer.module.coerceAssignableValue(item, elem)
			if err != nil {
				return fmt.Errorf("array element %d: %w", i, err)
			}
			prepared[i] = pointer.module.cloneValueForStore(normalized)
		}
		for i, value := range prepared {
			value = pointer.module.assignPreparedValue(pointer.array.valueAt(i), value)
			if err := pointer.array.setValueAt(i, value); err != nil {
				return fmt.Errorf("array element %d: %w", i, err)
			}
		}
		return nil
	}
	if pointer.slot == nil {
		return errors.New("invalid pointer")
	}
	if len(pointer.path) == 0 {
		if mutation {
			pointer.slot.publish(value)
			return nil
		}
		return pointer.slot.store(value)
	}
	updated, err := storeAddressPathMode(pointer.slot.module, pointer.slot.load(), pointer.path, pointer.indexes, value, !mutation)
	if err != nil {
		return err
	}
	pointer.slot.publish(updated)
	return nil
}

func commitPointerMutation(pointerValueInput, value vmValue) error {
	pointer, err := pointerValue(pointerValueInput)
	if err != nil {
		return err
	}
	return pointer.storeValue(value, true)
}

func pointerType(elem vmType) vmType {
	return runtimeTypeFromText("Ptr<" + elem.String() + ">")
}

func derefPointer(value vmValue) (vmValue, error) {
	pointer, err := pointerValue(value)
	if err != nil {
		return vmValue{}, err
	}
	return pointer.loadValue()
}

func storePointer(pointerValueInput, value vmValue) error {
	pointer, err := pointerValue(pointerValueInput)
	if err != nil {
		return err
	}
	return pointer.storeValue(value, false)
}

func pointerValue(value vmValue) (*vmPointer, error) {
	if value.Data == nil && value.Type.ShapeKind() == types.Pointer {
		return nil, newGuestPanic(errors.New("invalid memory address or nil pointer dereference"))
	}
	pointer, ok := value.Data.(*vmPointer)
	if !ok {
		return nil, fmt.Errorf("expected pointer payload for %s, got %T", value.Type, value.Data)
	}
	if pointer == nil {
		return nil, newGuestPanic(errors.New("invalid memory address or nil pointer dereference"))
	}
	if pointer.original == nil && pointer.target == pointerSlot && pointer.slot == nil {
		return nil, errors.New("invalid pointer")
	}
	return pointer, nil
}
