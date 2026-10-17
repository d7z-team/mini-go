package runtime

import (
	"errors"

	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

type managedCellStorage struct {
	heap   *managedHeap
	ref    guestRef
	typ    vmType
	layout preparedValueLayout
	owner  *vm
}

func (s *slot) ensureManagedCell(batches ...*managedAllocationBatch) (*managedCellStorage, error) {
	if s == nil {
		return nil, errors.New("nil slot")
	}
	if s.managedCell != nil {
		return s.managedCell, nil
	}
	module := s.module
	if module == nil && s.directOwner != nil {
		module = s.directOwner.module
	}
	if module == nil {
		return nil, errors.New("slot has no module owner")
	}
	value, initialized := s.snapshot()
	if !initialized {
		value = vmValue{Type: s.typ}
		if s.typ.ShapeKind() != types.Array && s.typ.ShapeKind() != types.Struct {
			value = module.zeroValue(s.typ)
		}
	}
	cell, err := module.newManagedCell(s.typ, value, batches...)
	if err != nil {
		return nil, err
	}
	s.managedCell = cell
	// Once the stable cell is published, the managed page owns the value. The
	// slot remains as an identity/access handle and must not retain an owning
	// host mirror that could diverge from the page.
	s.value = vmValue{}
	s.initialized = false
	return cell, nil
}

func (module *moduleInstance) newManagedCell(typ vmType, value vmValue, batches ...*managedAllocationBatch) (*managedCellStorage, error) {
	if module == nil || module.vm == nil {
		return nil, errors.New("managed cell has no owner")
	}
	if module.vm.heap == nil {
		module.vm.heap = newManagedHeap()
	}
	if module.vm.valueLayouts == nil {
		module.vm.valueLayouts = newPreparedLayoutRegistry()
	}
	typ = module.resolvedRuntimeType(typ)
	layout, err := module.vm.valueLayouts.layoutFor(typ)
	if err != nil {
		return nil, err
	}
	heapLayout, err := module.vm.heap.registerLayout(managedLayout{stride: layout.Stride, typeID: layout.ID, references: layout.References, dynamics: layout.Dynamics, types: layout.TypeOffsets})
	if err != nil {
		return nil, err
	}
	defer module.vm.heap.releaseLayout(heapLayout)
	var batch *managedAllocationBatch
	if len(batches) != 0 {
		batch = batches[0]
	}
	logical, valid := bytecode.CheckedStorageBytes(bytecode.StorageRecordHeaderBytes, 1, layout.Logical)
	if !valid {
		return nil, errManagedLimit
	}
	reservation, err := module.vm.heap.reserveWithBatch(batch, heapLayout, 1, logical)
	if err != nil {
		return nil, err
	}
	shape := layout.Shape
	if shape == types.Invalid {
		shape = typ.ShapeKind()
	}
	if shape == types.Any || shape == types.Interface {
		value = newVMValue(typ, value)
	} else if !value.Type.Valid() {
		value = vmValue{Type: typ}
		if shape != types.Array && shape != types.Struct {
			value = module.zeroValue(typ)
		}
	} else {
		value.Type = typ
	}
	if err := module.writeInlineValueWithLayout(reservation.bytes(), 0, value, layout, map[uintptr]guestRef{}, batch); err != nil {
		reservation.abort()
		return nil, err
	}
	ref := reservation.commit()
	if !ref.valid() {
		return nil, errors.New("managed cell publication failed")
	}
	dataType, ok := module.vm.valueLayouts.typeByID(layout.ID)
	if !ok {
		return nil, errors.New("managed cell type is unavailable")
	}
	cell := &managedCellStorage{heap: module.vm.heap, ref: ref, typ: dataType, layout: layout, owner: module.vm}
	return cell, nil
}

func (cell *managedCellStorage) write(value vmValue) error {
	if cell == nil || cell.owner == nil {
		return errors.New("managed cell is unavailable")
	}
	if cell.layout.Shape == types.Any || cell.layout.Shape == types.Interface {
		module := cell.owner.rootModule()
		if module == nil {
			module = &moduleInstance{vm: cell.owner}
		}
		encoded, err := module.dynamicEncoding(value, map[uintptr]guestRef{})
		if err != nil {
			return err
		}
		dynamicLayout, err := cell.owner.valueLayouts.layoutFor(encoded.typ)
		if err != nil {
			return err
		}
		var raw [24]byte
		if err := encodeManagedDynamic(raw[:], dynamicLayout.ID, encoded); err != nil {
			return err
		}
		return cell.heap.write(cell.ref, 0, raw[:])
	}
	return writeManagedInlineAt(cell.heap, cell.ref, 0, cell.typ, cell.layout, cell.owner.valueLayouts, cell.owner, value)
}

func (cell *managedCellStorage) read() (vmValue, error) {
	if cell == nil || cell.owner == nil {
		return vmValue{}, errors.New("managed cell is unavailable")
	}
	var value vmValue
	if cell.layout.Shape == types.Any || cell.layout.Shape == types.Interface {
		var raw [24]byte
		if err := cell.heap.read(cell.ref, 0, raw[:]); err != nil {
			return vmValue{}, err
		}
		if _, err := decodeManagedDynamicWithRegistry(raw[:], cell.typ, cell.heap, cell.owner.valueLayouts); err != nil {
			return vmValue{}, err
		}
		// Keep the declared Any/interface type on the cell handle. The dynamic
		// descriptor is decoded lazily by payload(), which preserves the wrapper
		// semantics needed by interface assertions and equality while retaining
		// the concrete type ID in the page.
		value = vmValue{Type: cell.typ, ref: cell.ref, heap: cell.heap, owner: cell.owner, layouts: cell.owner.valueLayouts}
	} else {
		value = decodeManagedInline(cell.heap, cell.ref, 0, cell.typ, cell.layout, cell.owner.valueLayouts, cell.owner)
	}
	value.module = cell.owner.rootModule()
	return value, nil
}
