package runtime

import (
	"encoding/binary"
	"errors"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// The revision anchor owns the global cells used by its code. Function
// records link to this page; the revision directory itself is not a GC root.
// Constants remain immutable prepared input and are imported when loaded.
func (revision *instanceRevision) ensureManagedAnchor() (guestRef, error) {
	if revision == nil || revision.owner == nil || revision.owner.heap == nil {
		return guestRef{}, nil
	}
	revision.anchorMu.Lock()
	defer revision.anchorMu.Unlock()
	if revision.anchor.valid() {
		return revision.anchor, nil
	}
	if revision.anchorReclaimed {
		return guestRef{}, errors.New("code revision has been reclaimed")
	}
	var cells []guestRef
	if revision.modules != nil {
		for _, module := range revision.modules.modules {
			for _, cell := range module.globalCells {
				storage, err := cell.ensureManagedCell()
				if err != nil {
					return guestRef{}, err
				}
				cells = append(cells, storage.ref)
			}
		}
	}
	heap := revision.owner.heap
	layout, err := heap.registerLayout(managedLayout{stride: 8, references: []uint32{0}})
	if err != nil {
		return guestRef{}, err
	}
	defer heap.releaseLayout(layout)
	logical, valid := ir.CheckedStorageBytes(ir.StorageRecordHeaderBytes, uint64(len(cells)), ir.StorageReferenceBytes)
	if !valid {
		return guestRef{}, errManagedLimit
	}
	reservation, err := heap.reserve(layout, uint32(len(cells)), logical)
	if err != nil {
		return guestRef{}, err
	}
	for index, ref := range cells {
		offset := index * 8
		binary.LittleEndian.PutUint32(reservation.bytes()[offset:], ref.slot)
		binary.LittleEndian.PutUint32(reservation.bytes()[offset+4:], ref.generation)
	}
	anchor := reservation.commit()
	if !anchor.valid() {
		return guestRef{}, errors.New("revision anchor publication failed")
	}
	revision.anchor = anchor
	revision.owner.managedRevisionsMu.Lock()
	if revision.owner.managedRevisions == nil {
		revision.owner.managedRevisions = make(map[guestRef]*instanceRevision)
	}
	revision.owner.managedRevisions[anchor] = revision
	revision.owner.managedRevisionsMu.Unlock()
	return anchor, nil
}

// Called under the heap owner lock, after mutators have stopped. The callback
// only drops side metadata; code/pool cleanup happens after collection.
func (vm *vm) reclaimManagedMetadata(ref guestRef) {
	vm.reclaimReflectTypeOwner(ref)
	vm.managedFunctionsMu.Lock()
	delete(vm.managedFunctions, ref)
	vm.managedFunctionsMu.Unlock()
	vm.managedWaitablesMu.Lock()
	delete(vm.managedWaitables, ref)
	vm.managedWaitablesMu.Unlock()
	vm.managedRevisionsMu.Lock()
	if revision := vm.managedRevisions[ref]; revision != nil {
		revision.anchorReclaimed = true
		delete(vm.managedRevisions, ref)
	}
	vm.managedRevisionsMu.Unlock()
}

// Major tracing has finished and reclaim callbacks have removed dead revision
// anchors. Metadata liveness comes from remaining code owners and numeric
// type lanes, rather than retaining every layout ever seen by this instance.
func (vm *vm) reclaimManagedLayouts(epoch uint64) uint64 {
	if vm.valueLayouts == nil {
		return 0
	}
	markRevision := func(revision *instanceRevision) {
		if revision == nil || revision.code == nil {
			return
		}
		for _, module := range revision.code.modules {
			vm.valueLayouts.markPreparedTypes(module.StorageTypeKeys, epoch)
		}
	}
	current := vm.revision.Load()
	markRevision(current)
	vm.managedRevisionsMu.Lock()
	for _, revision := range vm.managedRevisions {
		if revision != current {
			markRevision(revision)
		}
	}
	vm.managedRevisionsMu.Unlock()
	if vm.machine != nil {
		for _, task := range vm.machine.tasks {
			if task == nil {
				continue
			}
			for _, active := range task.retainedFrames {
				if active == nil || active.frame == nil {
					continue
				}
				frame := active.frame
				for bank := range 2 {
					layouts, offsets, initialized, raw := frame.inlineLayouts, frame.inlineOffsets, frame.inlineInitialized, frame.inlineStorage
					if bank == 1 {
						layouts, offsets, initialized, raw = frame.tempLayouts, frame.tempOffsets, frame.tempInitialized, frame.tempStorage
					}
					for index, layout := range layouts {
						if index >= len(initialized) || !initialized[index] {
							continue
						}
						// Large aggregate temporaries are published through their
						// managed reference in slotValues; they deliberately have no
						// raw frame lane to inspect here.
						if bank == 1 && index < len(frame.tempHeap) && frame.tempHeap[index] {
							continue
						}
						base := int(offsets[index])
						if base < 0 || base+int(layout.Stride) > len(raw) {
							continue
						}
						vm.valueLayouts.mark(layout.ID, epoch)
						for _, offset := range layout.TypeOffsets {
							position := base + int(offset)
							if position+4 <= len(raw) {
								vm.valueLayouts.mark(binary.LittleEndian.Uint32(raw[position:]), epoch)
							}
						}
						for _, offset := range layout.Dynamics {
							position := base + int(offset)
							if position+4 <= len(raw) {
								vm.valueLayouts.mark(binary.LittleEndian.Uint32(raw[position:]), epoch)
							}
						}
					}
				}
			}
		}
	}
	return vm.valueLayouts.sweep(epoch)
}
