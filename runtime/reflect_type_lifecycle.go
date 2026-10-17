package runtime

import "strings"

// trackReflectTypeOwner links a published reflect.Type record to the dynamic
// metadata it names. The metadata cache is only a lookup index; this ticket is
// the ownership edge that keeps the metadata live through managed tracing.
func (vm *vm) trackReflectTypeOwner(value vmValue, key string) {
	if vm == nil || !value.ref.valid() || value.heap != vm.heap || strings.TrimSpace(key) == "" {
		return
	}
	key = strings.TrimSpace(key)
	vm.reflectMetadataMu.Lock()
	defer vm.reflectMetadataMu.Unlock()
	if vm.reflectTypeOwners == nil {
		vm.reflectTypeOwners = make(map[guestRef]string)
		vm.reflectTypeOwnerCounts = make(map[string]uint32)
	}
	if previous, exists := vm.reflectTypeOwners[value.ref]; exists {
		if previous == key {
			return
		}
		if count := vm.reflectTypeOwnerCounts[previous]; count > 1 {
			vm.reflectTypeOwnerCounts[previous] = count - 1
		} else {
			delete(vm.reflectTypeOwnerCounts, previous)
		}
	}
	vm.reflectTypeOwners[value.ref] = key
	vm.reflectTypeOwnerCounts[key]++
}

// reclaimReflectTypeOwner runs from the heap reclaim callback while the heap
// lock is held. It only updates side metadata and queues the logical control
// bytes for release; releasing through the heap here would deadlock.
func (vm *vm) reclaimReflectTypeOwner(ref guestRef) {
	if vm == nil || !ref.valid() {
		return
	}
	vm.reflectMetadataMu.Lock()
	defer vm.reflectMetadataMu.Unlock()
	delete(vm.reflectTypeOwnerCandidates, ref)
	key, exists := vm.reflectTypeOwners[ref]
	if !exists {
		return
	}
	delete(vm.reflectTypeOwners, ref)
	if count := vm.reflectTypeOwnerCounts[key]; count > 1 {
		vm.reflectTypeOwnerCounts[key] = count - 1
		return
	}
	delete(vm.reflectTypeOwnerCounts, key)
	cost, dynamic := vm.reflectDynamicTypeCosts[key]
	if !dynamic {
		return
	}
	delete(vm.reflectDynamicTypeCosts, key)
	vm.reflectTypes.delete(key)
	vm.reflectTypeValues.delete(key)
	if vm.dynamicTypeCount > 0 {
		vm.dynamicTypeCount--
	}
	if cost > 0 {
		if cost > vm.dynamicTypeBytes {
			cost = vm.dynamicTypeBytes
		}
		vm.dynamicTypeBytes -= cost
		vm.reflectMetadataReleases += cost
	}
}

// drainReflectMetadataReleases is called after managed collection has dropped
// the heap owner lock. Dynamic reflection metadata uses the same control
// ledger as other runtime-owned state, so its bytes must be released through
// the normal heap path rather than adjusting counters directly.
func (vm *vm) drainReflectMetadataReleases() {
	if vm == nil {
		return
	}
	vm.reflectMetadataMu.Lock()
	// Inline encoding can publish a concrete record before its enclosing
	// aggregate is committed. If that aggregate later aborts, no reclaim
	// callback is emitted for the reserved ticket; prune only those candidate
	// owner edges before deciding which metadata is unowned. Published records
	// are removed from this set by their heap reclaim callback.
	for ref := range vm.reflectTypeOwnerCandidates {
		delete(vm.reflectTypeOwnerCandidates, ref)
		key, exists := vm.reflectTypeOwners[ref]
		if !exists {
			continue
		}
		if vm.heap == nil {
			delete(vm.reflectTypeOwners, ref)
		} else if _, err := vm.heap.resolveObject(ref); err == nil {
			continue
		} else {
			delete(vm.reflectTypeOwners, ref)
		}
		if count := vm.reflectTypeOwnerCounts[key]; count > 1 {
			vm.reflectTypeOwnerCounts[key] = count - 1
		} else {
			delete(vm.reflectTypeOwnerCounts, key)
		}
	}
	// A dynamic type constructor may finish without publishing its Type into
	// guest storage. The cache is deliberately not a root, so an ownerless
	// entry is discarded at the same collection boundary as dead managed
	// records. This also prevents detached cache values from keeping metadata
	// alive indefinitely.
	for key, cost := range vm.reflectDynamicTypeCosts {
		if vm.reflectTypeOwnerCounts[key] != 0 {
			continue
		}
		delete(vm.reflectDynamicTypeCosts, key)
		vm.reflectTypes.delete(key)
		vm.reflectTypeValues.delete(key)
		if vm.dynamicTypeCount > 0 {
			vm.dynamicTypeCount--
		}
		if cost > vm.dynamicTypeBytes {
			cost = vm.dynamicTypeBytes
		}
		vm.dynamicTypeBytes -= cost
		vm.reflectMetadataReleases += cost
	}
	pending := vm.reflectMetadataReleases
	vm.reflectMetadataReleases = 0
	vm.reflectMetadataMu.Unlock()
	vm.releaseAllocationBytes(pending)
}

func (vm *vm) trackReflectTypeValue(value vmValue) {
	if vm == nil || (value.Type.String() != "reflect.Type" && value.Type.String() != "reflect.runtimeType") {
		return
	}
	key, err := reflectTypeKeyFromValue(value)
	if err != nil {
		return
	}
	// A reflect.Type interface is represented by a dynamic slot. When the
	// wrapper is published, the concrete runtimeType record is its precise
	// owner; the wrapper may be an inline frame value with no guest ticket.
	candidate := value
	if inner, ok := value.Data.(vmValue); ok {
		candidate = inner
	} else if value.ref.valid() {
		if inner, ok := materializeManagedDynamic(value); ok && inner.ref.valid() {
			candidate = inner
		}
	}
	if candidate.ref.valid() {
		vm.trackReflectTypeOwner(candidate, key)
	}
}

// trackReflectTypeEncoding records the concrete record created while encoding
// an inline reflect.Type field. Inline frame storage has no outer guest ticket,
// but its dynamic representation points at the concrete runtimeType record.
func (vm *vm) trackReflectTypeEncoding(value vmValue, encoded managedValueEncoding) {
	if vm == nil || value.Type.String() != "reflect.Type" ||
		(encoded.representation != managedValueRef && encoded.representation != managedValueBox) ||
		!encoded.ref.valid() {
		return
	}
	key, err := reflectTypeKeyFromValue(value)
	if err != nil {
		return
	}
	vm.trackReflectTypeOwner(vmValue{
		Type: encoded.typ, ref: encoded.ref, heap: vm.heap, owner: vm,
		layouts: vm.valueLayouts,
	}, key)
	vm.reflectMetadataMu.Lock()
	if vm.reflectTypeOwnerCandidates == nil {
		vm.reflectTypeOwnerCandidates = make(map[guestRef]struct{})
	}
	vm.reflectTypeOwnerCandidates[encoded.ref] = struct{}{}
	vm.reflectMetadataMu.Unlock()
}

func (vm *vm) recordDynamicTypeCost(key string, bytes int64) {
	if vm == nil || strings.TrimSpace(key) == "" || bytes <= 0 {
		return
	}
	vm.reflectMetadataMu.Lock()
	if vm.reflectDynamicTypeCosts == nil {
		vm.reflectDynamicTypeCosts = make(map[string]int64)
	}
	vm.reflectDynamicTypeCosts[key] = bytes
	vm.reflectMetadataMu.Unlock()
}

func (vm *vm) clearReflectTypeMetadata() {
	if vm == nil {
		return
	}
	vm.reflectMetadataMu.Lock()
	vm.reflectTypeOwners = nil
	vm.reflectTypeOwnerCounts = nil
	vm.reflectTypeOwnerCandidates = nil
	vm.reflectDynamicTypeCosts = nil
	vm.reflectMetadataReleases = 0
	vm.reflectMetadataMu.Unlock()
}
