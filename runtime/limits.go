package runtime

import (
	"errors"
	"fmt"
	"sync/atomic"

	artifact "github.com/d7z-team/mini-go/runtime/bytecode"
)

type guestCensusRequest struct{ bytes int64 }

func (request *guestCensusRequest) Error() string { return "guest memory census required" }

func findGuestCensusRequest(err error) (*guestCensusRequest, bool) {
	for err != nil {
		if request, ok := err.(*guestCensusRequest); ok {
			return request, true
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() error }:
			err = wrapped.Unwrap()
		case interface{ Unwrap() []error }:
			for _, nested := range wrapped.Unwrap() {
				if request, ok := findGuestCensusRequest(nested); ok {
					return request, true
				}
			}
			return nil, false
		default:
			return nil, false
		}
	}
	return nil, false
}

func allocationLimitError(limit int64) ResourceLimitError {
	return ResourceLimitError{Code: "execution.allocation_limit", Message: fmt.Sprintf("execution allocation byte limit exceeded: max %d", limit)}
}

func (vm *vm) validateRuntimeValue(value vmValue) error {
	switch data := value.Data.(type) {
	case string:
		if len(data) > vm.limits.MaxStringBytes {
			return ResourceLimitError{Code: "execution.string_limit", Message: fmt.Sprintf("execution string byte limit exceeded: max %d", vm.limits.MaxStringBytes)}
		}
	case *vmArray:
		if data.Len > vm.limits.MaxCollectionElements {
			return ResourceLimitError{Code: "execution.collection_limit", Message: fmt.Sprintf("execution collection element limit exceeded: max %d", vm.limits.MaxCollectionElements)}
		}
	case *vmStruct:
		if data != nil && data.schema != nil && len(data.schema.fields) > vm.limits.MaxCollectionElements {
			return ResourceLimitError{Code: "execution.collection_limit", Message: fmt.Sprintf("execution collection element limit exceeded: max %d", vm.limits.MaxCollectionElements)}
		}
	case *vmSlice:
		if data != nil && data.Len > vm.limits.MaxCollectionElements {
			return ResourceLimitError{Code: "execution.collection_limit", Message: fmt.Sprintf("execution collection element limit exceeded: max %d", vm.limits.MaxCollectionElements)}
		}
	case *vmMap:
		if data != nil && data.length() > vm.limits.MaxCollectionElements {
			return ResourceLimitError{Code: "execution.collection_limit", Message: fmt.Sprintf("execution collection element limit exceeded: max %d", vm.limits.MaxCollectionElements)}
		}
	}
	return nil
}

func (vm *vm) chargeAllocation() error {
	return vm.chargeAllocationBytes(artifact.RuntimeNodeBytes)
}

func (vm *vm) chargeRuntimeObject(slots, mapEntries int) error {
	if slots < 0 || mapEntries < 0 {
		return ResourceLimitError{Code: "execution.allocation_limit", Message: "execution allocation size overflow"}
	}
	logicalBytes := artifact.RuntimeNodeBytes + int64(slots)*artifact.RuntimeSlotBytes + int64(mapEntries)*artifact.RuntimeMapEntryBytes
	if logicalBytes < 0 {
		return ResourceLimitError{Code: "execution.allocation_limit", Message: "execution allocation size overflow"}
	}
	return vm.chargeAllocationBytes(logicalBytes)
}

func (vm *vm) checkCollectionSize(length, capacity int64) (int, int, error) {
	if length < 0 || capacity < length {
		return 0, 0, newGuestPanic(fmt.Errorf("invalid collection length/capacity %d/%d", length, capacity))
	}
	maxInt := int64(^uint(0) >> 1)
	if length > maxInt || capacity > maxInt {
		return 0, 0, newGuestPanic(fmt.Errorf("collection length/capacity out of range: %d/%d", length, capacity))
	}
	if vm != nil && vm.limits.MaxCollectionElements > 0 && capacity > int64(vm.limits.MaxCollectionElements) {
		return 0, 0, ResourceLimitError{
			Code:    "execution.collection_limit",
			Message: fmt.Sprintf("execution collection element limit exceeded: max %d", vm.limits.MaxCollectionElements),
		}
	}
	return int(length), int(capacity), nil
}

func (vm *vm) chargeAllocationBytes(logicalBytes int64) error {
	if logicalBytes < 0 {
		return ResourceLimitError{Code: "execution.allocation_limit", Message: "execution allocation size overflow"}
	}
	if vm == nil || logicalBytes == 0 {
		return nil
	}
	limit := vm.limits.MaxAllocatedBytes
	if limit == 0 {
		limit = defaultLimits.MaxAllocatedBytes
	}
	refreshed := false
	for {
		allocated := vm.allocatedSinceSweep.Load()
		current := vm.liveGuestBytes.Load() + allocated
		if logicalBytes > limit-current {
			if !refreshed && vm.owner.Load() {
				vm.refreshLiveGuestBytes()
				refreshed = true
				continue
			}
			vm.leaseMu.Lock()
			running := vm.activeSlices != 0
			vm.leaseMu.Unlock()
			if running {
				return &guestCensusRequest{bytes: logicalBytes}
			}
			return allocationLimitError(limit)
		}
		if vm.allocatedSinceSweep.CompareAndSwap(allocated, allocated+logicalBytes) {
			addAtomicSaturating(&vm.totalAllocatedBytes, logicalBytes)
			updateAtomicMaximum(&vm.peakGuestBytes, current+logicalBytes)
			return nil
		}
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

func updateAtomicMaximum(value *atomic.Int64, candidate int64) {
	for current := value.Load(); candidate > current; current = value.Load() {
		if value.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func addAtomicSaturating(value *atomic.Int64, delta int64) {
	for current := value.Load(); ; current = value.Load() {
		next := current + delta
		if delta > 0 && next < current {
			next = int64(^uint64(0) >> 1)
		}
		if value.CompareAndSwap(current, next) {
			return
		}
	}
}

const (
	defaultPollQuantum     = 64 * 1024
	taskInstructionQuantum = 1024
)

var defaultLimits = Limits{
	MaxSteps:              100_000_000,
	MaxCallDepth:          1024,
	MaxTasks:              4096,
	MaxAllocatedBytes:     8 << 30,
	MaxStringBytes:        64 << 20,
	MaxCollectionElements: 1_000_000,
	MaxPendingEvents:      65_536,
	MaxBoundaryDepth:      128,
	MaxBoundaryBytes:      64 << 20,
	MaxRetainedRevisions:  128,
	MaxDynamicTypes:       4096,
	MaxDynamicTypeBytes:   16 << 20,
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
		{"MaxAllocatedBytes", limits.MaxAllocatedBytes},
		{"MaxStringBytes", int64(limits.MaxStringBytes)},
		{"MaxCollectionElements", int64(limits.MaxCollectionElements)},
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
	if limits.MaxAllocatedBytes == 0 {
		limits.MaxAllocatedBytes = defaultLimits.MaxAllocatedBytes
	}
	if limits.MaxStringBytes == 0 {
		limits.MaxStringBytes = defaultLimits.MaxStringBytes
	}
	if limits.MaxCollectionElements == 0 {
		limits.MaxCollectionElements = defaultLimits.MaxCollectionElements
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
