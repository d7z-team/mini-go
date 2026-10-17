package runtime

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

const managedWaitableIdentityBytes = 40

type managedWaitableStorage struct {
	heap   *managedHeap
	ref    guestRef
	elem   vmType
	layout preparedValueLayout
	owner  *vm
	batch  *managedAllocationBatch
}

func (storage *managedWaitableStorage) currentModule() *moduleInstance {
	if storage == nil || storage.owner == nil {
		return nil
	}
	return storage.owner.runtimeModule()
}

func (module *moduleInstance) importManagedWaitable(value vmValue, resource *waitableResource) (vmValue, error) {
	if resource == nil {
		return value, nil
	}
	result, err := module.newManagedWaitableWithBatch(value.Type, resource.ElemType, resource.Capacity, nil)
	if err != nil {
		return vmValue{}, err
	}
	view, ok := materializeManagedWaitable(result)
	if !ok {
		return vmValue{}, errors.New("managed waitable view unavailable")
	}
	completed := false
	defer func() {
		if !completed {
			view.managed.rollback()
		}
	}()
	items := resource.Buffer[resource.bufferHead:]
	if resource.managed != nil {
		var err error
		items, err = resource.managed.snapshotValues()
		if err != nil {
			return vmValue{}, err
		}
	}
	for _, item := range items {
		if err := view.managed.append(item); err != nil {
			return vmValue{}, err
		}
	}
	if resource.Closed {
		if err := view.managed.setClosed(true); err != nil {
			return vmValue{}, err
		}
	}
	completed = true
	return result, nil
}

func (module *moduleInstance) newManagedWaitableWithBatch(typ, elem vmType, capacity int, batch *managedAllocationBatch) (vmValue, error) {
	if module == nil || module.vm == nil {
		return vmValue{}, errors.New("managed waitable has no owner")
	}
	if module.vm.heap == nil {
		module.vm.heap = newManagedHeap()
	}
	if module.vm.valueLayouts == nil {
		module.vm.valueLayouts = newPreparedLayoutRegistry()
	}
	var err error
	elem, err = ownedStorageType(elem, module.resolvedRuntimeType)
	if err != nil {
		return vmValue{}, err
	}
	layout, err := module.vm.valueLayouts.layoutFor(elem)
	if err != nil {
		return vmValue{}, err
	}
	typeLayout, err := module.vm.valueLayouts.layoutFor(typ)
	if err != nil {
		return vmValue{}, err
	}
	identityLayout, err := module.vm.heap.registerLayout(managedLayout{stride: managedWaitableIdentityBytes, typeID: typeLayout.ID, references: []uint32{0}})
	if err != nil {
		return vmValue{}, err
	}
	defer module.vm.heap.releaseLayout(identityLayout)
	reservation, err := module.vm.heap.reserveWithBatch(batch, identityLayout, 1, bytecode.StorageRecordHeaderBytes+bytecode.StorageChannelHeaderBytes)
	if err != nil {
		return vmValue{}, err
	}
	ref := reservation.commit()
	if !ref.valid() {
		return vmValue{}, errors.New("managed waitable publication failed")
	}
	storage := &managedWaitableStorage{heap: module.vm.heap, ref: ref, elem: elem, layout: layout, owner: module.vm, batch: batch}
	completed := false
	defer func() {
		if !completed {
			rollbackManagedPublications(storage.heap, storage.batch, storage.ref)
		}
	}()
	if capacity < 0 {
		capacity = 0
	}
	if err := storage.initialize(uint64(capacity)); err != nil {
		return vmValue{}, err
	}
	completed = true
	return vmValue{Type: typ, ref: ref, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}, nil
}

func (storage *managedWaitableStorage) initialize(capacity uint64) error {
	var backing guestRef
	if capacity != 0 {
		logical, valid := bytecode.CheckedStorageBytes(bytecode.StorageRecordHeaderBytes, capacity, storage.layout.Logical)
		if !valid || capacity > math.MaxUint32 || capacity*uint64(storage.layout.Stride) > math.MaxUint32 {
			return errManagedLimit
		}
		layoutID, err := storage.heap.registerLayout(managedLayout{
			stride: storage.layout.Stride, typeID: storage.layout.ID,
			references: storage.layout.References, dynamics: storage.layout.Dynamics, types: storage.layout.TypeOffsets,
		})
		if err != nil {
			return err
		}
		defer storage.heap.releaseLayout(layoutID)
		reservation, err := storage.heap.reserveWithBatch(storage.batch, layoutID, uint32(capacity), logical)
		if err != nil {
			return err
		}
		backing = reservation.commit()
		if !backing.valid() {
			return errors.New("managed waitable backing publication failed")
		}
	}
	if err := storage.writeState(backing, 0, 0, capacity, false); err != nil {
		rollbackManagedPublications(storage.heap, storage.batch, backing)
		return err
	}
	return nil
}

func (storage *managedWaitableStorage) rollback() {
	if storage == nil || storage.heap == nil || !storage.ref.valid() {
		return
	}
	refs := []guestRef{storage.ref}
	if backing, _, _, _, _, err := storage.readState(); err == nil {
		refs = append(refs, backing)
	}
	rollbackManagedPublications(storage.heap, storage.batch, refs...)
}

func (storage *managedWaitableStorage) readState() (guestRef, uint64, uint64, uint64, bool, error) {
	var raw [managedWaitableIdentityBytes]byte
	if err := storage.heap.read(storage.ref, 0, raw[:]); err != nil {
		return guestRef{}, 0, 0, 0, false, err
	}
	return guestRef{slot: binary.LittleEndian.Uint32(raw[0:4]), generation: binary.LittleEndian.Uint32(raw[4:8])},
		binary.LittleEndian.Uint64(raw[8:16]), binary.LittleEndian.Uint64(raw[16:24]), binary.LittleEndian.Uint64(raw[24:32]), raw[32] != 0, nil
}

func (storage *managedWaitableStorage) writeState(backing guestRef, head, length, capacity uint64, closed bool) error {
	var raw [managedWaitableIdentityBytes]byte
	binary.LittleEndian.PutUint32(raw[0:4], backing.slot)
	binary.LittleEndian.PutUint32(raw[4:8], backing.generation)
	binary.LittleEndian.PutUint64(raw[8:16], head)
	binary.LittleEndian.PutUint64(raw[16:24], length)
	binary.LittleEndian.PutUint64(raw[24:32], capacity)
	if closed {
		raw[32] = 1
	}
	return storage.heap.write(storage.ref, 0, raw[:])
}

func (storage *managedWaitableStorage) length() int {
	_, _, length, _, _, err := storage.readState()
	if err != nil {
		return 0
	}
	return int(length)
}

func (storage *managedWaitableStorage) setClosed(closed bool) error {
	backing, head, length, capacity, _, err := storage.readState()
	if err != nil {
		return err
	}
	return storage.writeState(backing, head, length, capacity, closed)
}

func (storage *managedWaitableStorage) append(value vmValue, batches ...*managedAllocationBatch) error {
	backing, head, length, capacity, closed, err := storage.readState()
	if err != nil {
		return err
	}
	if closed || length >= capacity || !backing.valid() {
		return errors.New("managed waitable buffer is full")
	}
	module := storage.currentModule()
	if module == nil {
		return errors.New("managed waitable owner module is unavailable")
	}
	var scratch [maxInlineAggregateBytes]byte
	raw := scratch[:min(len(scratch), int(storage.layout.Stride))]
	if int(storage.layout.Stride) > len(scratch) {
		raw = make([]byte, storage.layout.Stride)
	}
	value.Type = storage.elem
	batch := storage.batch
	if len(batches) != 0 {
		batch = batches[0]
	}
	if err := module.writeInlineValueWithLayout(raw, 0, value, storage.layout, map[uintptr]guestRef{}, batch); err != nil {
		return err
	}
	index := (head + length) % capacity
	if err := storage.heap.write(backing, uint32(index)*storage.layout.Stride, raw); err != nil {
		return err
	}
	return storage.writeState(backing, head, length+1, capacity, closed)
}

func (storage *managedWaitableStorage) pop(batches ...*managedAllocationBatch) (vmValue, error) {
	backing, head, length, capacity, closed, err := storage.readState()
	if err != nil {
		return vmValue{}, err
	}
	if length == 0 || !backing.valid() {
		return vmValue{}, errors.New("managed waitable buffer is empty")
	}
	value := decodeManagedInline(storage.heap, backing, uint32(head%capacity)*storage.layout.Stride, storage.elem, storage.layout, storage.owner.valueLayouts, storage.owner)
	module := storage.currentModule()
	value.module = module
	if storage.elem.ShapeKind() == types.Array || storage.elem.ShapeKind() == types.Struct {
		// The ring can be cleared or reused immediately after receive. Acquire
		// independent value storage before changing the buffer's ownership.
		batch := storage.batch
		if len(batches) != 0 {
			batch = batches[0]
		}
		cell, err := module.newManagedCell(storage.elem, value, batch)
		if err != nil {
			return vmValue{}, err
		}
		value.ref, value.offset = cell.ref, 0
	}
	oldHead, oldLength := head, length
	if length == 1 {
		head = 0
	} else {
		head = (head + 1) % capacity
	}
	if err := storage.writeState(backing, head, length-1, capacity, closed); err != nil {
		return vmValue{}, err
	}
	if err := storage.heap.clear(backing, uint32(oldHead%capacity)*storage.layout.Stride, storage.layout.Stride); err != nil {
		_ = storage.writeState(backing, oldHead, oldLength, capacity, closed)
		return vmValue{}, err
	}
	return value, nil
}

func (storage *managedWaitableStorage) snapshotValues() ([]vmValue, error) {
	if storage == nil {
		return nil, errors.New("managed waitable is unavailable")
	}
	backing, head, length, capacity, _, err := storage.readState()
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, nil
	}
	if !backing.valid() || capacity == 0 || length > capacity {
		return nil, errors.New("managed waitable state is invalid")
	}
	values := make([]vmValue, 0, length)
	for index := uint64(0); index < length; index++ {
		value := decodeManagedInline(storage.heap, backing, uint32((head+index)%capacity)*storage.layout.Stride, storage.elem, storage.layout, storage.owner.valueLayouts, storage.owner)
		value.module = storage.currentModule()
		values = append(values, value)
	}
	return values, nil
}

func materializeManagedWaitable(value vmValue) (*waitableResource, bool) {
	if value.heap == nil || !value.ref.valid() || value.Type.ShapeKind() != types.Waitable || value.owner == nil {
		return nil, false
	}
	module := value.module
	if module == nil {
		module = value.owner.runtimeModule()
	}
	if module == nil {
		return nil, false
	}
	// The resource can outlive the revision that created it. Clone the
	// complete waitable type graph before storing it in the host-side resource
	// index, then use the same owned element type for the managed payload.
	ownedType, err := ownedStorageType(value.Type, module.resolvedRuntimeType)
	if err != nil {
		return nil, false
	}
	info, ok := ownedType.WaitableInfo()
	if !ok {
		return nil, false
	}
	elem := info.Elem
	layout, err := value.layouts.layoutFor(elem)
	if err != nil {
		return nil, false
	}
	storage := &managedWaitableStorage{heap: value.heap, ref: value.ref, elem: elem, layout: layout, owner: value.owner}
	_, _, _, capacity, closed, err := storage.readState()
	if err != nil {
		return nil, false
	}
	resource := &waitableResource{Type: ownedType, ElemType: elem, Capacity: int(capacity), Closed: closed, managed: storage}
	value.owner.managedWaitablesMu.Lock()
	if existing := value.owner.managedWaitables[value.ref]; existing != nil {
		value.owner.managedWaitablesMu.Unlock()
		return existing, true
	}
	if value.owner.managedWaitables == nil {
		value.owner.managedWaitables = make(map[guestRef]*waitableResource)
	}
	value.owner.managedWaitables[value.ref] = resource
	value.owner.managedWaitablesMu.Unlock()
	return resource, true
}
