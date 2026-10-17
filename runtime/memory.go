package runtime

import "encoding/binary"

// managedMajorDue distinguishes old/control growth from short-lived nursery
// allocations. Hard admission pressure can require reclaiming both generations.
func (vm *vm) managedMajorDue() bool {
	if vm == nil || vm.heap == nil {
		return false
	}
	heap := vm.heap
	heap.mu.Lock()
	defer heap.mu.Unlock()
	used := heap.counters.occupiedBytes + heap.counters.reservedBytes
	if heap.counters.occupiedBytes-heap.youngBytes >= heap.nextMajorBytes {
		return true
	}
	if heap.maxGuestBytes == 0 {
		return false
	}
	remaining := heap.maxGuestBytes - min(heap.maxGuestBytes, used)
	return used >= heap.maxGuestBytes || remaining < managedBatchCredit
}

func (vm *vm) collectManagedAtSafepoint() error {
	if vm == nil || vm.heap == nil || !vm.heap.collectionRequested.Load() {
		return nil
	}
	if vm.managedMajorDue() {
		return vm.collectMajorManaged()
	}
	return vm.collectMinorManaged()
}

func (vm *vm) collectMajorManaged() error {
	if vm == nil || vm.heap == nil {
		return nil
	}
	_, err := vm.heap.collectFromRoots(vm.visitManagedRoots, true)
	if err == nil {
		vm.drainReflectMetadataReleases()
	}
	return err
}

func (vm *vm) collectMinorManaged() error {
	if vm == nil || vm.heap == nil {
		return nil
	}
	_, err := vm.heap.collectFromRoots(vm.visitManagedRoots, false)
	if err == nil {
		vm.drainReflectMetadataReleases()
	}
	return err
}

// visitManagedRoots reads frozen owner state without materializing guest
// aggregates or building host snapshots. Collector mark bits deduplicate roots.
func (vm *vm) visitManagedRoots(visit func(guestRef)) {
	if vm == nil {
		return
	}
	visitLayout := func(layout preparedValueLayout, raw []byte) {
		for _, offset := range layout.References {
			if uint64(offset)+8 <= uint64(len(raw)) {
				visit(guestRef{slot: binary.LittleEndian.Uint32(raw[offset:]), generation: binary.LittleEndian.Uint32(raw[offset+4:])})
			}
		}
		for _, offset := range layout.Dynamics {
			if uint64(offset)+24 > uint64(len(raw)) {
				continue
			}
			representation := managedValueRepresentation(binary.LittleEndian.Uint32(raw[offset+4:]))
			if representation == managedValueRef || representation == managedValueBox {
				visit(guestRef{slot: binary.LittleEndian.Uint32(raw[offset+8:]), generation: binary.LittleEndian.Uint32(raw[offset+12:])})
			}
		}
	}
	var visitValue func(vmValue)
	visitValue = func(value vmValue) {
		if value.ref.valid() && value.heap == vm.heap {
			visit(value.ref)
			return
		}
		if owner := value.inlineOwner; owner != nil {
			var layout preparedValueLayout
			var raw []byte
			var ok bool
			if value.inlineTemp {
				layout, raw, ok = owner.tempLayout(value.inlineIndex)
			} else {
				layout, raw, ok = owner.inlineLayout(value.inlineIndex)
			}
			if ok {
				visitLayout(layout, raw)
			}
			return
		}
		switch data := value.Data.(type) {
		case vmValue:
			visitValue(data)
		case *vmSlice:
			if data != nil && data.managedHeap == vm.heap {
				visit(data.managedRef)
			}
		case *vmArray:
			if data != nil && data.managedHeap == vm.heap {
				visit(data.managedRef)
			}
		case *vmStruct:
			if data != nil && data.managedHeap == vm.heap {
				visit(data.managedRef)
			}
		}
	}
	visitCell := func(cell *slot) {
		if cell == nil {
			return
		}
		if cell.managedCell != nil {
			visit(cell.managedCell.ref)
		} else if owner := cell.inlineOwner; owner != nil {
			index := cell.inlineIndex
			if index >= 0 && index < len(owner.inlineInitialized) && owner.inlineInitialized[index] {
				if layout, raw, ok := owner.inlineLayout(index); ok {
					visitLayout(layout, raw)
				}
			}
		} else if cell.directOwner == nil {
			if value, initialized := cell.snapshot(); initialized {
				visitValue(value)
			}
		}
	}
	visitSelection := func(selection *channelSelection) {
		if selection == nil {
			return
		}
		visitValue(selection.value)
		for _, selected := range selection.cases {
			visitValue(selected.channel)
			visitValue(selected.value)
			visitValue(selected.zero)
		}
	}
	for _, value := range vm.controlRoots {
		visitValue(value)
	}
	revision := vm.revision.Load()
	if revision != nil {
		if revision.anchor.valid() {
			visit(revision.anchor)
		} else if revision.modules != nil {
			for _, module := range revision.modules.modules {
				if module == nil || module.state == nil {
					continue
				}
				for _, cell := range module.state.globals {
					visitCell(cell)
				}
			}
		}
	}
	vm.retiredMu.Lock()
	for _, revision := range vm.retiredRevisions {
		revision.mu.Lock()
		pinned := revision.pins != 0
		revision.mu.Unlock()
		if pinned && revision.anchor.valid() {
			visit(revision.anchor)
		}
	}
	vm.retiredMu.Unlock()
	if machine := vm.machine; machine != nil {
		for _, task := range machine.tasks {
			if task == nil {
				continue
			}
			visitSelection(task.preparingSelection)
			if blocked := task.blocked; blocked != nil {
				if blocked.mutex != nil && blocked.mutex.resource != nil {
					visit(blocked.mutex.resource.ref)
				}
				visitSelection(blocked.selection)
				if blocked.reflectSelect != nil {
					visitSelection(blocked.reflectSelect.selection)
				}
				if blocked.reflectRecv != nil {
					visitValue(blocked.reflectRecv.waitable)
				}
			}
			for _, value := range task.sliceValues {
				visitValue(value)
			}
			for _, active := range task.retainedFrames {
				if active == nil || active.frame == nil {
					continue
				}
				frame := active.frame
				for index, cell := range frame.localCells {
					if frame.referenceLive(index) {
						visitCell(cell)
					}
				}
				for index, cell := range frame.upvalueCells {
					if frame.referenceLive(len(frame.localCells) + len(frame.function.Decl.Code.Types) + index) {
						visitCell(cell)
					}
				}
				for index, value := range frame.slotValues {
					if !frame.referenceLive(len(frame.localCells) + index) {
						continue
					}
					if value.inlineOwner != nil {
						visitValue(value)
						continue
					}
					if index < len(frame.tempLayouts) && frame.tempLayouts[index].Stride != 0 && !frame.tempHeap[index] {
						if frame.tempInitialized[index] {
							if layout, raw, ok := frame.tempLayout(index); ok {
								visitLayout(layout, raw)
							}
						}
					} else {
						visitValue(value)
					}
				}
				for _, value := range frame.popValues {
					visitValue(value)
				}
				for _, value := range frame.returnValues {
					visitValue(value)
				}
				if active.completionActive {
					for _, value := range active.completion.returnValues {
						visitValue(value)
					}
					if active.completion.panic != nil {
						visitValue(active.completion.panic.err.Value)
					}
				}
				if frame.typeDispatchActive {
					visitValue(frame.typeDispatchValue)
				}
				for _, deferred := range frame.defers {
					for _, captured := range deferred.ref.captureSlots {
						visitCell(captured)
					}
				}
				for _, iterator := range frame.mapIteratorSlots {
					if iterator != nil {
						visitValue(iterator.object)
					}
				}
				if active.recoveredPanic != nil {
					visitValue(active.recoveredPanic.err.Value)
				}
			}
		}
	}
	for _, timer := range vm.timers {
		if timer != nil {
			visitValue(timer.signal)
		}
	}
}
