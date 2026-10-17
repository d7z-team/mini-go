package runtime

func (vm *vm) sweepRetiredRevisions() {
	if vm == nil {
		return
	}
	retired := vm.retiredRevisionSnapshot()
	if len(retired) == 0 {
		return
	}
	if vm.retentionCollectionRequested.Swap(false) && vm.heap != nil {
		if err := vm.collectMajorManaged(); err != nil {
			vm.retentionCollectionRequested.Store(true)
		}
	}
	for _, revision := range retired {
		revision.setRootRetained(revision.anchor.valid() && !revision.anchorReclaimed)
	}
	vm.retiredMu.Lock()
	kept := vm.retiredRevisions[:0]
	for _, revision := range vm.retiredRevisions {
		revision.mu.Lock()
		closed := revision.closed
		revision.mu.Unlock()
		if !closed {
			kept = append(kept, revision)
		}
	}
	clear(vm.retiredRevisions[len(kept):])
	vm.retiredRevisions = kept
	vm.retiredMu.Unlock()
}

func (vm *vm) retiredRevisionSnapshot() []*instanceRevision {
	if vm == nil {
		return nil
	}
	vm.retiredMu.Lock()
	defer vm.retiredMu.Unlock()
	return append([]*instanceRevision(nil), vm.retiredRevisions...)
}

func (vm *vm) addRetiredRevision(revision *instanceRevision) {
	if vm == nil || revision == nil {
		return
	}
	vm.retiredMu.Lock()
	vm.retiredRevisions = append(vm.retiredRevisions, revision)
	vm.retiredMu.Unlock()
}

func (vm *vm) removeRetiredRevision(target *instanceRevision) {
	if vm == nil || target == nil {
		return
	}
	vm.retiredMu.Lock()
	defer vm.retiredMu.Unlock()
	for index, revision := range vm.retiredRevisions {
		if revision != target {
			continue
		}
		copy(vm.retiredRevisions[index:], vm.retiredRevisions[index+1:])
		vm.retiredRevisions[len(vm.retiredRevisions)-1] = nil
		vm.retiredRevisions = vm.retiredRevisions[:len(vm.retiredRevisions)-1]
		return
	}
}

func (vm *vm) liveRevisionCount() int {
	if vm == nil {
		return 0
	}
	count := 0
	if current := vm.revision.Load(); current != nil {
		current.mu.Lock()
		if !current.closed {
			count++
		}
		current.mu.Unlock()
	}
	for _, revision := range vm.retiredRevisionSnapshot() {
		if revision == nil {
			continue
		}
		revision.mu.Lock()
		if !revision.closed {
			count++
		}
		revision.mu.Unlock()
	}
	return count
}

func (vm *vm) checkRevisionCapacity() error {
	if limit := vm.limits.MaxRetainedRevisions; limit > 0 && vm.liveRevisionCount()+1 > limit {
		return PatchError{Code: "resource_limit", Message: "retained revision limit exceeded"}
	}
	return nil
}

func (vm *vm) closeRevisions() {
	if vm == nil {
		return
	}
	current := vm.revision.Swap(nil)
	if current != nil {
		current.close()
	}
	vm.retiredMu.Lock()
	retired := vm.retiredRevisions
	vm.retiredRevisions = nil
	vm.retiredMu.Unlock()
	for _, revision := range retired {
		revision.close()
	}
	vm.paused = nil
	vm.reflectTypes.clear()
	vm.reflectTypeValues.clear()
	vm.clearReflectTypeMetadata()
	vm.dynamicTypeCount = 0
	vm.dynamicTypeBytes = 0
	vm.timers = nil
	vm.ffiCalls = nil
	vm.machine = nil
	if vm.heap != nil {
		vm.heap.close()
	}
	vm.managedFunctions = nil
	vm.managedWaitables = nil
	vm.managedRevisions = nil
	vm.valueLayouts = nil
	vm.pendingBoundaryBytes.Store(0)
}
