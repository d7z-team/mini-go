package runtime

import (
	"errors"
	"fmt"
)

func allocationLimitError(limit int64) ResourceLimitError {
	return ResourceLimitError{Code: "execution.allocation_limit", Message: fmt.Sprintf("execution allocation byte limit exceeded: max %d", limit)}
}

func (vm *vm) recordAllocationFailure(err error) {
	if vm == nil || err == nil {
		return
	}
	if errors.Is(err, errManagedCollection) && vm.heap != nil {
		// A worker can only report allocation pressure. The owner observes this
		// bit at the next scheduler boundary and performs the major collection
		// required to reclaim dead old/control records.
		vm.heap.collectionRequested.Store(true)
	}
	vm.allocationFailureMu.Lock()
	vm.allocationFailure = err
	vm.allocationFailureMu.Unlock()
}

func (vm *vm) takeAllocationFailure() error {
	if vm == nil {
		return nil
	}
	vm.allocationFailureMu.Lock()
	err := vm.allocationFailure
	vm.allocationFailure = nil
	vm.allocationFailureMu.Unlock()
	return err
}

func (vm *vm) checkCollectionSize(length, capacity int64) (int, int, error) {
	if length < 0 || capacity < length {
		return 0, 0, newGuestPanic(fmt.Errorf("invalid collection length/capacity %d/%d", length, capacity))
	}
	maxInt := int64(^uint(0) >> 1)
	if length > maxInt || capacity > maxInt {
		return 0, 0, newGuestPanic(fmt.Errorf("collection length/capacity out of range: %d/%d", length, capacity))
	}
	return int(length), int(capacity), nil
}

func (vm *vm) chargeAllocationBytes(logicalBytes int64, batches ...*managedAllocationBatch) error {
	reservation, err := vm.reserveControlBytes(logicalBytes, batches...)
	if err != nil {
		return err
	}
	reservation.commit()
	return nil
}

func (vm *vm) reserveControlBytes(logicalBytes int64, batches ...*managedAllocationBatch) (managedControlReservation, error) {
	if logicalBytes < 0 {
		return managedControlReservation{}, ResourceLimitError{Code: "execution.allocation_limit", Message: "execution allocation size overflow"}
	}
	if vm == nil || logicalBytes == 0 {
		return managedControlReservation{}, nil
	}
	if vm.heap == nil {
		// Unit-level embedders may construct a VM directly. Initialize the
		// same managed ledger used by normal instances before admitting any
		// control storage; this keeps the accounting path single-sourced.
		vm.hostMu.Lock()
		if vm.heap == nil {
			heap := newManagedHeap()
			limits := normalizeLimits(vm.limits)
			heap.maxGuestBytes = uint64(limits.MaxGuestBytes)
			heap.maxResidentBytes = uint64(limits.MaxResidentBytes)
			vm.heap = heap
		}
		vm.hostMu.Unlock()
	}
	limit := normalizeLimits(vm.limits).MaxGuestBytes
	for attempt := 0; attempt < 2; attempt++ {
		reservation, err := vm.heap.reserveControl(uint64(logicalBytes), batches...)
		if err == nil {
			return reservation, nil
		}
		if err != errManagedCollection || attempt != 0 || !vm.owner.Load() {
			return managedControlReservation{}, allocationLimitError(limit)
		}
		if err := vm.collectMajorManaged(); err != nil {
			if errors.Is(err, errManagedCollectorBusy) {
				// The current owner slice still owns an allocation lease. Defer
				// collection until the scheduler flushes that lease, then retry the
				// exact instruction at its safepoint.
				vm.heap.collectionRequested.Store(true)
				return managedControlReservation{}, errManagedCollection
			}
			return managedControlReservation{}, err
		}
	}
	return managedControlReservation{}, allocationLimitError(limit)
}

func (vm *vm) releaseAllocationBytes(logicalBytes int64) {
	if vm == nil || logicalBytes <= 0 {
		return
	}
	if vm.heap != nil {
		vm.heap.releaseControl(uint64(logicalBytes))
	}
}

func (vm *vm) reservePendingEvent() error {
	if vm == nil {
		return errors.New("external call requires an active VM")
	}
	for {
		count := vm.pendingEvents.Load()
		if limit := vm.limits.MaxPendingEvents; limit > 0 && count >= int64(limit) {
			return fmt.Errorf("pending event limit exceeded: max %d", limit)
		}
		if vm.pendingEvents.CompareAndSwap(count, count+1) {
			return nil
		}
	}
}

func (vm *vm) reserveBoundaryBytes(logicalBytes int64) error {
	if logicalBytes < 0 {
		return ResourceLimitError{Code: "execution.boundary_limit", Message: "pending boundary size overflow"}
	}
	if vm == nil || logicalBytes == 0 {
		return nil
	}
	limit := vm.limits.MaxBoundaryBytes
	for {
		current := vm.pendingBoundaryBytes.Load()
		if logicalBytes < 0 || current > limit-logicalBytes {
			return ResourceLimitError{Code: "execution.boundary_limit", Message: fmt.Sprintf("pending boundary byte limit exceeded: max %d", limit)}
		}
		if vm.pendingBoundaryBytes.CompareAndSwap(current, current+logicalBytes) {
			return nil
		}
	}
}

func (vm *vm) releasePendingEvent(logicalBytes int64) {
	if vm == nil {
		return
	}
	vm.pendingEvents.Add(-1)
	if logicalBytes > 0 {
		vm.pendingBoundaryBytes.Add(-logicalBytes)
	}
}

// Larger Go slices amortize owner handoff and allocation-batch setup in
// CPU-bound task workloads; explicit PollSteps budgets still bound each call.
const (
	defaultPollQuantum     = 8192
	taskInstructionQuantum = 8192
)

var defaultLimits = Limits{
	MaxSteps:             100_000_000,
	MaxCallDepth:         1024,
	MaxTasks:             4096,
	MaxGuestBytes:        8 << 30,
	MaxResidentBytes:     0,
	MaxPendingEvents:     65_536,
	MaxBoundaryDepth:     128,
	MaxBoundaryBytes:     64 << 20,
	MaxRetainedRevisions: 128,
	MaxDynamicTypes:      4096,
	MaxDynamicTypeBytes:  16 << 20,
}

// UnlimitedSteps disables the cumulative instruction budget for each scope.
const UnlimitedSteps int64 = -1

func validateLimits(limits Limits) error {
	values := []struct {
		name  string
		value int64
	}{
		{"MaxSteps", limits.MaxSteps},
		{"MaxCallDepth", int64(limits.MaxCallDepth)},
		{"MaxTasks", int64(limits.MaxTasks)},
		{"MaxGuestBytes", limits.MaxGuestBytes},
		{"MaxResidentBytes", limits.MaxResidentBytes},
		{"MaxPendingEvents", int64(limits.MaxPendingEvents)},
		{"MaxBoundaryDepth", int64(limits.MaxBoundaryDepth)},
		{"MaxBoundaryBytes", limits.MaxBoundaryBytes},
		{"MaxRetainedRevisions", int64(limits.MaxRetainedRevisions)},
		{"MaxDynamicTypes", int64(limits.MaxDynamicTypes)},
		{"MaxDynamicTypeBytes", limits.MaxDynamicTypeBytes},
	}
	for _, value := range values {
		if value.value < 0 && !(value.name == "MaxSteps" && value.value == UnlimitedSteps) {
			return fmt.Errorf("runtime limit %s cannot be negative", value.name)
		}
	}
	return nil
}

func normalizeLimits(limits Limits) Limits {
	if limits.MaxSteps == 0 {
		limits.MaxSteps = defaultLimits.MaxSteps
	}
	if limits.MaxCallDepth == 0 {
		limits.MaxCallDepth = defaultLimits.MaxCallDepth
	}
	if limits.MaxTasks == 0 {
		limits.MaxTasks = defaultLimits.MaxTasks
	}
	if limits.MaxGuestBytes == 0 {
		limits.MaxGuestBytes = defaultLimits.MaxGuestBytes
	}
	if limits.MaxPendingEvents == 0 {
		limits.MaxPendingEvents = defaultLimits.MaxPendingEvents
	}
	if limits.MaxBoundaryDepth == 0 {
		limits.MaxBoundaryDepth = defaultLimits.MaxBoundaryDepth
	}
	if limits.MaxBoundaryBytes == 0 {
		limits.MaxBoundaryBytes = defaultLimits.MaxBoundaryBytes
	}
	if limits.MaxRetainedRevisions == 0 {
		limits.MaxRetainedRevisions = defaultLimits.MaxRetainedRevisions
	}
	if limits.MaxDynamicTypes == 0 {
		limits.MaxDynamicTypes = defaultLimits.MaxDynamicTypes
	}
	if limits.MaxDynamicTypeBytes == 0 {
		limits.MaxDynamicTypeBytes = defaultLimits.MaxDynamicTypeBytes
	}
	return limits
}
