package runtime

import (
	"errors"
	"fmt"
	"math"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// A pointer register carries its target record and byte offset by value.
// Typed storage adds the target type ID to that address. Slot/path objects are
// construction inputs and never retain code or frames afterwards.
type managedPointerLocation struct {
	ref    guestRef
	offset uint32
	typ    vmType
}

func (module *moduleInstance) newManagedPointer(pointer *vmPointer) (vmValue, error) {
	location, err := module.resolveManagedPointer(pointer)
	if err != nil {
		return vmValue{}, err
	}
	targetType := module.resolvedRuntimeType(pointer.Type)
	return vmValue{Type: pointerType(targetType), ref: location.ref, offset: location.offset, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}, nil
}

func (module *moduleInstance) resolveManagedPointer(pointer *vmPointer) (managedPointerLocation, error) {
	var location managedPointerLocation
	if pointer == nil {
		return location, errors.New("nil managed pointer")
	}
	for pointer.original != nil {
		pointer = pointer.original
	}
	if pointer.storage.valid() {
		return managedPointerLocation{ref: pointer.storage, offset: pointer.offset, typ: pointer.Type}, nil
	}
	var current vmValue
	path, indexes := pointer.path, pointer.indexes
	switch pointer.target {
	case pointerCell, pointerSlot:
		cell := pointer.cell
		if cell == nil {
			cell = pointer.slot
		}
		storage, err := cell.ensureManagedCell()
		if err != nil {
			return location, err
		}
		location = managedPointerLocation{ref: storage.ref, typ: storage.typ}
		current, err = storage.read()
		if err != nil {
			return location, err
		}
	case pointerArray:
		if pointer.array != nil && pointer.array.managedHeap == nil {
			_, elem, ok := module.resolvedRuntimeType(pointer.Type).ArrayInfo()
			if !ok {
				return location, errors.New("array address has invalid element type")
			}
			value, err := module.importDetachedValue(newVMValue("Slice<"+elem.String()+">", pointer.array), map[uintptr]guestRef{})
			if err != nil {
				return location, err
			}
			managed, ok := value.payload().(*vmSlice)
			if !ok || managed == nil {
				return location, errors.New("array address has invalid slice backing")
			}
			*pointer.array = *managed
		}
		if pointer.array == nil || pointer.array.managedHeap != module.vm.heap || !pointer.array.managedRef.valid() {
			return location, errors.New("array address has no managed backing")
		}
		offset := uint64(pointer.array.managedBase) + uint64(pointer.array.Start)*uint64(pointer.array.managedElem.Stride)
		if offset > math.MaxUint32 {
			return location, errors.New("array address offset overflow")
		}
		return managedPointerLocation{ref: pointer.array.managedRef, offset: uint32(offset), typ: pointer.Type}, nil
	case pointerField, pointerIndex, pointerBackingPath:
		current = pointer.parent
		if current.Type.ShapeKind() == types.Pointer {
			parent, err := pointerValue(current)
			if err != nil {
				return location, err
			}
			location, err = module.resolveManagedPointer(parent)
			if err != nil {
				return location, err
			}
			current, err = parent.loadValue()
			if err != nil {
				return location, err
			}
		}
		switch pointer.target {
		case pointerField:
			path = []ir.AddressPathSegment{{Kind: "field", Field: pointer.field}}
		case pointerIndex:
			path = []ir.AddressPathSegment{{Kind: "index"}}
			indexes = []vmValue{newVMValue("Int", pointer.index)}
		}
	default:
		return location, errors.New("invalid managed pointer target")
	}
	indexPosition := 0
	for _, segment := range path {
		switch segment.Kind {
		case "indirect":
			parent, err := pointerValue(current)
			if err != nil {
				return location, err
			}
			location, err = module.resolveManagedPointer(parent)
			if err != nil {
				return location, err
			}
			current, err = parent.loadValue()
			if err != nil {
				return location, err
			}
		case "field":
			if current.Type.ShapeKind() == types.Pointer {
				var err error
				current, err = derefPointer(current)
				if err != nil {
					return location, err
				}
			}
			structure, ok := current.payload().(*vmStruct)
			if !ok || structure == nil || structure.managedHeap != module.vm.heap || !structure.managedRef.valid() {
				return location, fmt.Errorf("field address %q has no managed struct storage (type %s, kind %v, ref %v, payload %T)", segment.Field, current.Type, current.Type.ShapeKind(), current.ref, current.payload())
			}
			index, _, ok := structure.schema.field(segment.Field)
			if !ok || index >= len(structure.managedFields) {
				return location, fmt.Errorf("unknown address field %q", segment.Field)
			}
			field := structure.managedFields[index]
			location = managedPointerLocation{ref: structure.managedRef, offset: structure.managedBase + field.offset, typ: field.typ}
			current = decodeManagedInline(module.vm.heap, location.ref, location.offset, field.typ, field.layout, module.vm.valueLayouts, module.vm)
		case "index":
			if indexPosition >= len(indexes) {
				return location, errors.New("address index is missing")
			}
			index, err := asInt64(indexes[indexPosition])
			indexPosition++
			if err != nil {
				return location, err
			}
			if current.Type.ShapeKind() == types.Pointer {
				current, err = derefPointer(current)
				if err != nil {
					return location, err
				}
			}
			var backing *vmSlice
			switch value := current.payload().(type) {
			case *vmArray:
				backing = &value.vmSlice
			case *vmSlice:
				backing = value
			}
			if backing == nil || backing.managedHeap != module.vm.heap || !backing.managedRef.valid() {
				return location, errors.New("element address has no managed backing")
			}
			if index < 0 || index >= int64(backing.Len) {
				return location, newGuestPanic(fmt.Errorf("index out of range: %d", index))
			}
			offset := uint64(backing.managedBase) + uint64(int64(backing.Start)+index)*uint64(backing.managedElem.Stride)
			if offset > math.MaxUint32 {
				return location, errors.New("element address offset overflow")
			}
			location = managedPointerLocation{ref: backing.managedRef, offset: uint32(offset), typ: backing.managedType}
			current = decodeManagedInline(module.vm.heap, location.ref, location.offset, location.typ, backing.managedElem, module.vm.valueLayouts, module.vm)
		default:
			return location, fmt.Errorf("invalid address segment %q", segment.Kind)
		}
	}
	if !location.ref.valid() {
		return location, errors.New("managed pointer target has no storage")
	}
	return location, nil
}

func materializeManagedPointer(value vmValue) (*vmPointer, bool) {
	if value.Type.ShapeKind() != types.Pointer || value.heap == nil || !value.ref.valid() || value.owner == nil {
		return nil, false
	}
	typ, ok := value.Type.PointerElem()
	if !ok {
		return nil, false
	}
	module := value.module
	if module == nil {
		module = value.owner.rootModule()
	}
	if module != nil {
		typ = module.resolvedRuntimeType(typ)
	} else if resolved, ok := value.layouts.resolvedType(typ); ok {
		typ = resolved
	}
	return &vmPointer{Type: typ, module: module, owner: value.owner, storage: value.ref, offset: value.offset}, true
}
