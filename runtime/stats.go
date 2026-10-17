package runtime

import (
	"context"
	"errors"
)

// InstanceState is the externally observable lifecycle of an Instance.
type InstanceState string

const (
	InstanceOpen    InstanceState = "open"
	InstanceFaulted InstanceState = "faulted"
	InstanceClosing InstanceState = "closing"
	InstanceClosed  InstanceState = "closed"
)

// ScopeStats is an immutable snapshot of one invocation and its derived work.
type ScopeStats struct {
	ID        int64
	Started   RevisionInfo
	RootState ExecutionState
	Tasks     int
	Timers    int
	FFICalls  int
	Steps     int64
	Done      bool
	Err       error
}

// Stats is a point-in-time runtime snapshot copied by the VM owner.
type Stats struct {
	Revision                 RevisionInfo
	State                    InstanceState
	ActiveScopes             int
	RunnableTasks            int
	BlockedTasks             int
	BlockedContexts          []BlockedContext
	PausedTasks              int
	PendingFFICalls          int
	Timers                   int
	RetainedRevisions        int
	ExecutedSteps            int64
	OccupiedGuestBytes       int64
	ReservedGuestBytes       int64
	TotalGuestAllocatedBytes int64
	LastMajorReachableBytes  int64
	CopiedGuestBytes         int64
	PromotedGuestBytes       int64
	ResidentPageBytes        int64
	ManagedAllocations       int64
	MinorCollections         int64
	MajorCollections         int64
	GCNanos                  int64
	ScannedRoots             int64
	ScannedCards             int64
	ScannedReferences        int64
	PendingBoundaryBytes     int64
	DynamicTypes             int
	DynamicTypeBytes         int64
}

// State returns the current instance lifecycle without entering the VM owner.
func (i *Instance) State() InstanceState {
	switch i.lifecycleState() {
	case instanceOpen:
		return InstanceOpen
	case instanceFaulted:
		return InstanceFaulted
	case instanceClosing:
		return InstanceClosing
	default:
		return InstanceClosed
	}
}

// RuntimeStats returns a stable snapshot without exposing scheduler state.
func (i *Instance) RuntimeStats(ctx context.Context) (Stats, error) {
	if i == nil || i.vm == nil {
		return Stats{}, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := i.vm.enterOwnerContext(ctx); err != nil {
		return Stats{}, err
	}
	defer i.vm.leaveOwner()
	stats := Stats{
		State: i.State(), PendingFFICalls: len(i.vm.ffiCalls), Timers: len(i.vm.timers),
		RetainedRevisions: i.vm.liveRevisionCount(), ExecutedSteps: i.vm.executedSteps,
		PendingBoundaryBytes: i.vm.pendingBoundaryBytes.Load(),
		DynamicTypes:         i.vm.dynamicTypeCount, DynamicTypeBytes: i.vm.dynamicTypeBytes,
	}
	if i.vm.heap != nil {
		heap := i.vm.heap.stats()
		stats.OccupiedGuestBytes = int64(heap.occupiedBytes)
		stats.ReservedGuestBytes = int64(heap.reservedBytes)
		stats.LastMajorReachableBytes = int64(heap.lastMajorReachableBytes)
		stats.CopiedGuestBytes = int64(heap.copiedBytes)
		stats.PromotedGuestBytes = int64(heap.promotedBytes)
		stats.TotalGuestAllocatedBytes = int64(heap.totalAllocatedBytes)
		stats.ResidentPageBytes = int64(heap.residentPageBytes)
		stats.ManagedAllocations = int64(heap.allocations)
		stats.MinorCollections = int64(heap.minorCollections)
		stats.MajorCollections = int64(heap.majorCollections)
		stats.GCNanos = int64(heap.gcNanos)
		stats.ScannedRoots = int64(heap.scannedRoots)
		stats.ScannedCards = int64(heap.scannedCards)
		stats.ScannedReferences = int64(heap.scannedReferences)
	}
	if revision := i.vm.revision.Load(); revision != nil && revision.code != nil {
		stats.Revision = RevisionInfo{Generation: revision.generation, Hash: revision.code.image.Hash, SymbolsHash: revision.symbolsHash()}
	}
	if machine := i.vm.machine; machine != nil {
		stats.ActiveScopes = len(machine.scopes)
		stats.RunnableTasks = machine.runnableCount()
		stats.BlockedTasks, stats.BlockedContexts = machine.blockedContextSnapshot()
		if machine.paused != nil {
			stats.PausedTasks = 1
		}
	}
	return stats, nil
}
