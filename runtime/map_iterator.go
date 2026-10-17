package runtime

import (
	"fmt"
	"unsafe"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// A snapshot identifies entries, not language keys: NaN keys cannot be looked
// up by equality, and a deleted/reinserted key denotes a new entry.
type mapIterator struct {
	object        vmValue
	storage       *vmMap
	entries       []uint64
	position      int
	admittedBytes int64
	residentBytes uint64
}

// initMapIteratorAt is the prepared path. localIndex is stable for a loaded
// function, so the next operation can address the cursor without looking up
// its source name in a map.
func (f *frame) initMapIteratorAt(localIndex int, local string, object vmValue) error {
	if localIndex < 0 || localIndex >= len(f.mapIteratorSlots) {
		return fmt.Errorf("map iterator local %q is outside prepared frame", local)
	}
	if !f.module.isMapType(object.Type) {
		return fmt.Errorf("map iterator requires map, got %s", object.Type)
	}
	payload := object.payload()
	data, ok := payload.(*vmMap)
	if !ok && payload != nil {
		return fmt.Errorf("invalid map backing for %s", object.Type)
	}
	count := data.length()
	logicalBytes := ir.StorageControlRecordBytes + int64(count)*ir.StorageReferenceBytes
	reservation, err := f.module.vm.reserveControlBytes(logicalBytes, f.allocationBatch)
	if err != nil {
		return err
	}
	residentBytes := uint64(unsafe.Sizeof(mapIterator{})) + uint64(count)*8
	heap := f.module.vm.heap
	heap.mu.Lock()
	admitted := !heap.closed && heap.admitResident(residentBytes)
	heap.mu.Unlock()
	if !admitted {
		reservation.abort()
		return errManagedLimit
	}
	identities := data.entryIdentities()
	reservation.commit()
	if previous := f.mapIteratorSlots[localIndex]; previous != nil {
		previous.release(f.module.vm)
	}
	iterator := &mapIterator{object: object, storage: data, entries: identities, admittedBytes: logicalBytes, residentBytes: residentBytes}
	f.mapIteratorSlots[localIndex] = iterator
	return nil
}

func (f *frame) nextMapIteratorAt(localIndex int, local string) error {
	if localIndex < 0 || localIndex >= len(f.mapIteratorSlots) {
		return fmt.Errorf("map iterator local %q is outside prepared frame", local)
	}
	iterator := f.mapIteratorSlots[localIndex]
	if iterator == nil {
		return fmt.Errorf("map iterator %q is not initialized", local)
	}
	data := iterator.storage
	for iterator.position < len(iterator.entries) {
		candidate := iterator.entries[iterator.position]
		iterator.position++
		entry, exists := data.loadIdentity(candidate)
		if !exists {
			continue
		}
		if err := f.pushCopied(entry.Key); err != nil {
			return err
		}
		if err := f.pushCopied(entry.Value); err != nil {
			return err
		}
		f.push(newBoolValue(true))
		return nil
	}
	keyType, valueType, _ := f.module.mapKeyValueTypes(iterator.object.Type)
	f.push(f.module.zeroValue(keyType))
	f.push(f.module.zeroValue(valueType))
	f.push(newBoolValue(false))
	iterator.entries = nil
	iterator.release(f.module.vm)
	return nil
}

func (iterator *mapIterator) release(vm *vm) {
	if iterator == nil || iterator.admittedBytes == 0 {
		return
	}
	if vm != nil {
		vm.releaseAllocationBytes(iterator.admittedBytes)
		if heap := vm.heap; heap != nil {
			heap.mu.Lock()
			if !heap.closed {
				heap.counters.residentPageBytes -= iterator.residentBytes
			}
			heap.mu.Unlock()
		}
	}
	iterator.admittedBytes = 0
	iterator.residentBytes = 0
	iterator.entries = nil
}
