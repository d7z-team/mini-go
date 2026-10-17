package runtime

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	managedPageBytes   = uint32(64 * 1024)
	managedCardBytes   = uint32(256)
	managedSegmentSize = uint32(256)
	managedStripeCount = uint32(256)
	managedFree        = uint32(0)
	managedReserved    = uint32(1)
	managedPublished   = uint32(2)
	managedYoungPage   = uint8(1)
	managedSparePage   = uint8(2)
	managedOldPage     = uint8(3)
	managedLargePage   = uint8(4)
)

var (
	errManagedCollection = errors.New("managed allocation requires an owner collection")
	errManagedLimit      = errors.New("managed storage exceeds memory limit")
	errManagedReference  = errors.New("invalid or stale managed reference")
)

type guestRef struct{ slot, generation uint32 }

func (ref guestRef) valid() bool { return ref.slot != 0 && ref.generation != 0 }

// swap exchanges disjoint typed ranges in one backing without materializing
// aggregate views that could observe the first half of the update.
func (heap *managedHeap) swap(ref guestRef, left, right, size uint32) error {
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	defer stripe.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	if uint64(left)+uint64(size) > uint64(entry.size) || uint64(right)+uint64(size) > uint64(entry.size) {
		return errors.New("managed swap outside payload")
	}
	if left == right || size == 0 {
		return nil
	}
	if uint64(min(left, right))+uint64(size) > uint64(max(left, right)) {
		return errors.New("managed swap requires disjoint ranges")
	}
	data := heap.pageDirectory.Load().pages[entry.page].data
	left += entry.offset
	right += entry.offset
	var scratch [256]byte
	for remaining := size; remaining != 0; {
		count := min(remaining, uint32(len(scratch)))
		copy(scratch[:count], data[left:left+count])
		copy(data[left:left+count], data[right:right+count])
		copy(data[right:right+count], scratch[:count])
		left, right, remaining = left+count, right+count, remaining-count
	}
	if !entry.young {
		heap.dirtyRegion(entry, left-entry.offset-size, size)
		heap.dirtyRegion(entry, right-entry.offset-size, size)
	}
	return nil
}

// Offsets trace the actual bytes in one repeated element. Dynamic offsets
// identify 24-byte values; Ref/Box representations 4/5 contain a ticket at +8.
type managedLayout struct {
	stride     uint32
	typeID     uint32
	references []uint32
	dynamics   []uint32
	types      []uint32
}

type managedLayoutOwner struct {
	pins   uint32
	marked uint64
}

type managedObject struct {
	state                                         atomic.Uint32
	generation, layout, count, page, offset, size uint32
	logical, marked                               uint64
	occupiedIndex, youngIndex                     uint32
	age                                           uint8
	young                                         bool
}

type managedPage struct {
	data                 []byte
	entries, free        []uint32
	cards                []atomic.Uint32
	used, class, partner uint32
	cardCapacity         uint32
	kind                 uint8
	resident             uint64
	leased               bool
}

type (
	managedCard      struct{ page, index uint32 }
	managedDirectory struct {
		segments []*[managedSegmentSize]managedObject
	}
	managedPageDirectory   struct{ pages []*managedPage }
	managedLayoutDirectory struct{ layouts []managedLayout }
)

type managedHeapStats struct {
	occupiedBytes, reservedBytes, lastMajorReachableBytes uint64
	residentPageBytes, totalAllocatedBytes, allocations   uint64
	copiedBytes, promotedBytes, reclaimedBytes            uint64
	minorCollections, majorCollections                    uint64
	gcNanos                                               uint64
	scannedRoots, scannedCards, scannedReferences         uint64
}

type managedCollection struct {
	reachableBytes, reclaimedBytes, copiedBytes, promotedBytes uint64
	roots, cards, references                                   uint64
}

// Directory entries contain numeric locations only. Collection and explicit
// release require the scheduler's stopped-mutator gate. Published payload
// operations hold one of a fixed set of stripes, never an object-owned mutex.
type managedHeap struct {
	mu                                                       sync.Mutex
	stripes                                                  [managedStripeCount]sync.Mutex
	cardsMu                                                  sync.Mutex
	directory                                                atomic.Pointer[managedDirectory]
	pageDirectory                                            atomic.Pointer[managedPageDirectory]
	layoutDirectory                                          atomic.Pointer[managedLayoutDirectory]
	segments                                                 []*[managedSegmentSize]managedObject
	pages                                                    []*managedPage
	layouts                                                  []managedLayout
	layoutOwners                                             []managedLayoutOwner
	freePages, freeSlots, occupied, youngSlots, nurseryPages []uint32
	oldAvailable                                             [14][]uint32
	pendingCards                                             []managedCard
	queue                                                    []uint32
	nextSlot                                                 uint32
	epoch                                                    uint64
	unpublished                                              uint32
	activeBatches                                            uint32
	batchClasses                                             uint16
	maxGuestBytes, maxResidentBytes                          uint64
	controlBytes                                             uint64
	nurseryCapacity, nurseryLimit                            uint64
	youngBytes                                               uint64
	nextMajorBytes                                           uint64
	collectionRequested                                      atomic.Bool
	collectorWorkers                                         int
	counters                                                 managedHeapStats
	closed                                                   bool
	poisonCollected                                          bool
	// onReclaim only invalidates side metadata keyed by a published ticket. It
	// must not call back into the heap while the owner lock is held.
	onReclaim func(guestRef)
	onType    func(uint32, uint64)
	onMajor   func(uint64) uint64
}

const managedMajorGrowthBytes = 8 << 20

func newManagedHeap() *managedHeap {
	// The same collector supports bounded workers; single-worker execution
	// avoids queue and stripe contention for the measured VM object graphs.
	heap := &managedHeap{nextSlot: 1, nurseryLimit: 4 << 20, nextMajorBytes: managedMajorGrowthBytes, collectorWorkers: 1, layouts: []managedLayout{{}}, layoutOwners: []managedLayoutOwner{{}}}
	heap.directory.Store(&managedDirectory{})
	heap.pageDirectory.Store(&managedPageDirectory{})
	heap.layoutDirectory.Store(&managedLayoutDirectory{layouts: heap.layouts})
	return heap
}

// registerLayout acquires a construction pin. The caller releases it once
// payload publication succeeds or fails; published records then own the shape.
func (heap *managedHeap) registerLayout(layout managedLayout) (uint32, error) {
	if layout.stride == 0 {
		return 0, errors.New("managed layout requires a nonzero stride")
	}
	// Prepared layouts are already ordered. Borrow their immutable offsets
	// while looking up an existing shape; only a new shape needs owned slices.
	if !slices.IsSorted(layout.references) {
		layout.references = slices.Clone(layout.references)
		slices.Sort(layout.references)
	}
	if !slices.IsSorted(layout.dynamics) {
		layout.dynamics = slices.Clone(layout.dynamics)
		slices.Sort(layout.dynamics)
	}
	if !slices.IsSorted(layout.types) {
		layout.types = slices.Clone(layout.types)
		slices.Sort(layout.types)
	}
	for i, offset := range layout.types {
		if uint64(offset)+4 > uint64(layout.stride) || i != 0 && offset == layout.types[i-1] {
			return 0, errors.New("invalid managed type offset")
		}
	}
	for i, offset := range layout.references {
		if uint64(offset)+8 > uint64(layout.stride) || i != 0 && offset < layout.references[i-1]+8 {
			return 0, errors.New("invalid managed reference offset")
		}
	}
	for i, offset := range layout.dynamics {
		if uint64(offset)+24 > uint64(layout.stride) || i != 0 && offset < layout.dynamics[i-1]+24 {
			return 0, errors.New("invalid managed dynamic offset")
		}
		for _, reference := range layout.references {
			if uint64(reference) < uint64(offset)+24 && uint64(reference)+8 > uint64(offset) {
				return 0, errors.New("overlapping managed reference and dynamic storage")
			}
		}
	}
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if heap.closed {
		return 0, errors.New("cannot register layout on closed heap")
	}
	available := 0
	for id := 1; id < len(heap.layouts); id++ {
		previous := heap.layouts[id]
		if previous.stride == 0 && available == 0 {
			available = id
		}
		if previous.stride == layout.stride && previous.typeID == layout.typeID && slices.Equal(previous.references, layout.references) && slices.Equal(previous.dynamics, layout.dynamics) && slices.Equal(previous.types, layout.types) {
			heap.layoutOwners[id].pins++
			return uint32(id), nil
		}
	}
	bytes := uint64(len(layout.references)+len(layout.dynamics)+len(layout.types)) * 4
	if available == 0 {
		bytes += uint64(unsafe.Sizeof(managedLayout{}) + unsafe.Sizeof(managedLayoutOwner{}))
	}
	if !heap.admitResident(bytes) {
		return 0, errManagedLimit
	}
	layout.references = slices.Clone(layout.references)
	layout.dynamics = slices.Clone(layout.dynamics)
	layout.types = slices.Clone(layout.types)
	if available != 0 {
		heap.layouts[available] = layout
		heap.layoutOwners[available] = managedLayoutOwner{pins: 1}
		return uint32(available), nil
	}
	layouts := make([]managedLayout, len(heap.layouts)+1)
	copy(layouts, heap.layouts)
	layouts[len(heap.layouts)] = layout
	heap.layouts = layouts
	owners := make([]managedLayoutOwner, len(layouts))
	copy(owners, heap.layoutOwners)
	owners[len(owners)-1].pins = 1
	heap.layoutOwners = owners
	heap.layoutDirectory.Store(&managedLayoutDirectory{layouts: layouts})
	return uint32(len(layouts) - 1), nil
}

func (heap *managedHeap) releaseLayout(id uint32) {
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if !heap.closed && id != 0 && int(id) < len(heap.layoutOwners) && heap.layoutOwners[id].pins != 0 {
		heap.layoutOwners[id].pins--
	}
}

func (heap *managedHeap) admitResident(bytes uint64) bool {
	used := heap.counters.residentPageBytes
	if bytes > math.MaxInt64-used || heap.maxResidentBytes != 0 && bytes > heap.maxResidentBytes-min(heap.maxResidentBytes, used) {
		return false
	}
	heap.counters.residentPageBytes += bytes
	return true
}

func (heap *managedHeap) growDirectory() error {
	capacity := (len(heap.segments) + 1) * int(managedSegmentSize)
	// All live/free/young/mark vectors are admitted with the directory. Marking
	// cannot require a fallible worklist expansion after it has started.
	segmentCapacity := cap(heap.segments)
	if segmentCapacity < len(heap.segments)+1 {
		segmentCapacity = max(len(heap.segments)+1, segmentCapacity*2)
	}
	vectors := [...]*[]uint32{&heap.freeSlots, &heap.occupied, &heap.youngSlots, &heap.queue}
	var capacities [len(vectors)]int
	bytes := uint64(unsafe.Sizeof([managedSegmentSize]managedObject{})) + uint64(segmentCapacity-cap(heap.segments))*uint64(unsafe.Sizeof(uintptr(0)))
	for index, vector := range vectors {
		capacities[index] = cap(*vector)
		if capacities[index] < capacity {
			capacities[index] = max(capacity, capacities[index]*2)
		}
		bytes += uint64(capacities[index]-cap(*vector)) * 4
	}
	if !heap.admitResident(bytes) {
		return errManagedLimit
	}
	segments := heap.segments
	if segmentCapacity != cap(segments) {
		segments = make([]*[managedSegmentSize]managedObject, len(heap.segments), segmentCapacity)
		copy(segments, heap.segments)
	}
	// Readers keep immutable slice lengths. Appending a new segment only writes
	// beyond every previously published directory's visible range.
	segments = segments[:len(segments)+1]
	segments[len(heap.segments)] = new([managedSegmentSize]managedObject)
	heap.segments = segments
	for index, vector := range vectors {
		if capacities[index] == cap(*vector) {
			continue
		}
		next := make([]uint32, len(*vector), capacities[index])
		copy(next, *vector)
		*vector = next
	}
	heap.directory.Store(&managedDirectory{segments: segments})
	return nil
}

func managedPageCost(size uint32, kind uint8, class uint32) uint64 {
	bytes := uint64(size)
	switch kind {
	case managedOldPage:
		bytes += uint64(size/class)*8 + uint64((size+managedCardBytes-1)/managedCardBytes)*4
	case managedLargePage:
		bytes += 4 + (uint64(size)+uint64(managedCardBytes)-1)/uint64(managedCardBytes)*4
	}
	return bytes
}

// newPage is called under mu. Nursery callers pre-admit both semi-spaces.
// Old pages include exact size-class metadata and a bounded dirty-card log.
func (heap *managedHeap) newPage(size uint32, kind uint8, class uint32, preadmitted bool) (uint32, error) {
	cardCount := 0
	switch kind {
	case managedOldPage, managedLargePage:
		cardCount = int((uint64(size) + uint64(managedCardBytes) - 1) / uint64(managedCardBytes))
	}
	bytes := managedPageCost(size, kind, class)
	header := uint64(unsafe.Sizeof(managedPage{})) + uint64(unsafe.Sizeof(uintptr(0))) + 4 + 14*4
	cardCapacity := uint32(0)
	if len(heap.freePages) != 0 {
		header = 0
		cardCapacity = heap.pages[heap.freePages[len(heap.freePages)-1]].cardCapacity
	}
	cardGrowth := max(0, cardCount-int(cardCapacity))
	if !preadmitted && !heap.admitResident(bytes+header+uint64(cardGrowth)*uint64(unsafe.Sizeof(managedCard{}))) {
		return 0, errManagedLimit
	}
	var id uint32
	if len(heap.freePages) != 0 {
		id = heap.freePages[len(heap.freePages)-1]
		heap.freePages = heap.freePages[:len(heap.freePages)-1]
	} else {
		id = uint32(len(heap.pages))
		pages := make([]*managedPage, len(heap.pages)+1)
		copy(pages, heap.pages)
		pages[id] = new(managedPage)
		heap.pages = pages
		vectors := [15]*[]uint32{&heap.freePages}
		for index := range heap.oldAvailable {
			vectors[index+1] = &heap.oldAvailable[index]
		}
		for _, vector := range vectors {
			if cap(*vector) < len(pages) {
				next := make([]uint32, len(*vector), len(pages))
				copy(next, *vector)
				*vector = next
			}
		}
		heap.pageDirectory.Store(&managedPageDirectory{pages: pages})
	}
	page := heap.pages[id]
	*page = managedPage{data: make([]byte, int(size)), kind: kind, class: class, resident: bytes, cardCapacity: max(cardCapacity, uint32(cardCount))}
	switch kind {
	case managedOldPage:
		page.entries = make([]uint32, size/class)
		page.free = make([]uint32, 0, size/class)
		for index := size / class; index > 0; index-- {
			page.free = append(page.free, index-1)
		}
		heap.oldAvailable[bits.TrailingZeros32(class)-3] = append(heap.oldAvailable[bits.TrailingZeros32(class)-3], id)
	case managedLargePage:
		page.entries = make([]uint32, 1)
	}
	if cardCount != 0 {
		page.cards = make([]atomic.Uint32, cardCount)
	}
	if cardGrowth != 0 {
		heap.cardsMu.Lock()
		cards := make([]managedCard, len(heap.pendingCards), cap(heap.pendingCards)+cardGrowth)
		copy(cards, heap.pendingCards)
		heap.pendingCards = cards
		heap.cardsMu.Unlock()
	}
	return id, nil
}

type managedReservation struct {
	heap          *managedHeap
	batch         *managedAllocationBatch
	rollbackBatch *managedAllocationBatch
	ref           guestRef
	done          bool
}

// nurseryPage returns owner-held allocation space. A leased page is exclusive
// to one worker until its batch is returned, and is skipped by other allocators.
func (heap *managedHeap) nurseryPage(size uint32) (uint32, error) {
	for _, id := range heap.nurseryPages {
		page := heap.pages[id]
		if !page.leased && size <= uint32(len(page.data))-page.used {
			return id, nil
		}
	}
	if heap.nurseryCapacity+uint64(managedPageBytes) > heap.nurseryLimit {
		// Nursery size is a collection target, not a hard guest limit. Finish
		// the current instruction using admitted space, then stop at the next
		// instruction boundary. Hard resident/guest admission still applies.
		heap.collectionRequested.Store(true)
	}
	header := uint64(unsafe.Sizeof(managedPage{})) + uint64(unsafe.Sizeof(uintptr(0))) + 4 + 14*4
	pairBytes := 2*uint64(managedPageBytes) + uint64(max(0, 2-len(heap.freePages)))*header + 4
	if !heap.admitResident(pairBytes) {
		return 0, errManagedLimit
	}
	pageID, _ := heap.newPage(managedPageBytes, managedYoungPage, 0, true)
	spare, _ := heap.newPage(managedPageBytes, managedSparePage, 0, true)
	heap.pages[pageID].partner, heap.pages[spare].partner = spare, pageID
	pages := make([]uint32, len(heap.nurseryPages)+1)
	copy(pages, heap.nurseryPages)
	pages[len(heap.nurseryPages)] = pageID
	heap.nurseryPages = pages
	heap.nurseryCapacity += uint64(managedPageBytes)
	return pageID, nil
}

// managedBudgetTransfer records worker credit temporarily moved to an owner
// reservation. The heap lock is held by the caller while the transfer is
// active; rollback is therefore deliberately a small ledger-only operation.
// Keeping this state in one place makes payload and control reservations share
// the same guest-budget invariant without adding work to batch.reserve.
type managedBudgetTransfer struct {
	heap  *managedHeap
	batch *managedAllocationBatch
	bytes uint64
}

func (transfer *managedBudgetTransfer) rollback() {
	if transfer.bytes == 0 {
		return
	}
	transfer.batch.credit += transfer.bytes
	transfer.heap.counters.reservedBytes += transfer.bytes
	transfer.bytes = 0
}

// admitGuestBytes checks the shared occupied/reserved guest ledger and, when
// necessary, moves unused credit from the calling worker lease to the owner.
// The heap lock must be held by the caller. A returned transfer must be
// cleared after successful admission or rolled back before returning an error.
func (heap *managedHeap) admitGuestBytes(logical uint64, batches ...*managedAllocationBatch) (managedBudgetTransfer, error) {
	used := heap.counters.occupiedBytes + heap.counters.reservedBytes
	var transfer managedBudgetTransfer
	if heap.maxGuestBytes != 0 && logical > heap.maxGuestBytes-min(heap.maxGuestBytes, used) && len(batches) != 0 {
		batch := batches[0]
		if batch != nil && batch.heap == heap && !batch.finished {
			available := batch.credit - batch.committed - batch.reserved
			needed := logical - (heap.maxGuestBytes - min(heap.maxGuestBytes, used))
			released := min(available, needed)
			batch.credit -= released
			heap.counters.reservedBytes -= released
			used -= released
			transfer = managedBudgetTransfer{heap: heap, batch: batch, bytes: released}
		}
	}
	if used > math.MaxInt64 || logical > math.MaxInt64-used || heap.maxGuestBytes != 0 && logical > heap.maxGuestBytes-min(heap.maxGuestBytes, used) {
		transfer.rollback()
		return managedBudgetTransfer{}, errManagedCollection
	}
	return transfer, nil
}

func (heap *managedHeap) reserve(layout, count uint32, logicalBytes uint64, batches ...*managedAllocationBatch) (*managedReservation, error) {
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if heap.closed || layout == 0 || int(layout) >= len(heap.layouts) || heap.layouts[layout].stride == 0 {
		return nil, errors.New("invalid managed allocation layout or closed heap")
	}
	physical := uint64(heap.layouts[layout].stride) * uint64(count)
	if logicalBytes > math.MaxInt64 || physical > math.MaxUint32-7 {
		return nil, errManagedLimit
	}
	size := uint32(max(uint64(8), (physical+7)&^7))
	transfer, err := heap.admitGuestBytes(logicalBytes, batches...)
	if err != nil {
		return nil, err
	}
	defer transfer.rollback()
	for len(heap.freeSlots) != 0 && heap.object(heap.freeSlots[len(heap.freeSlots)-1]).generation == math.MaxUint32 {
		heap.freeSlots = heap.freeSlots[:len(heap.freeSlots)-1]
	}
	if len(heap.freeSlots) == 0 && heap.nextSlot/managedSegmentSize >= uint32(len(heap.segments)) {
		if heap.nextSlot == math.MaxUint32 {
			return nil, errManagedLimit
		}
		if err := heap.growDirectory(); err != nil {
			return nil, err
		}
	}
	var pageID, offset uint32
	young := size <= managedPageBytes/4
	if young {
		var err error
		pageID, err = heap.nurseryPage(size)
		if err != nil {
			return nil, err
		}
		offset = heap.pages[pageID].used
		class := uint32(1) << max(3, bits.Len32(size-1))
		if len(heap.oldAvailable[bits.TrailingZeros32(class)-3]) == 0 {
			// Optional promotion space is admitted before a collection, never
			// while copying. Failure leaves the guaranteed to-space available.
			_, _ = heap.newPage(managedPageBytes, managedOldPage, class, false)
		}
	} else {
		var err error
		pageID, err = heap.newPage(size, managedLargePage, 0, false)
		if err != nil {
			return nil, err
		}
	}
	var slot uint32
	if len(heap.freeSlots) != 0 {
		slot = heap.freeSlots[len(heap.freeSlots)-1]
		heap.freeSlots = heap.freeSlots[:len(heap.freeSlots)-1]
	} else {
		slot, heap.nextSlot = heap.nextSlot, heap.nextSlot+1
	}
	entry := heap.object(slot)
	stripe := &heap.stripes[slot%managedStripeCount]
	stripe.Lock()
	entry.generation++
	entry.layout, entry.count, entry.page, entry.offset, entry.size = layout, count, pageID, offset, size
	entry.logical, entry.age, entry.young, entry.marked = logicalBytes, 0, young, 0
	entry.state.Store(managedReserved)
	stripe.Unlock()
	page := heap.pages[pageID]
	clear(page.data[offset : offset+size])
	page.used += size
	if !young {
		page.entries[0] = slot
	}
	heap.counters.reservedBytes += logicalBytes
	heap.unpublished++
	transfer.bytes = 0
	return &managedReservation{heap: heap, ref: guestRef{slot: slot, generation: entry.generation}}, nil
}

// reserveWithBatch uses a worker-owned nursery lease when the object fits in
// that lease. The fallback is intentionally the existing owner reservation:
// a lease is an allocation fast path, not a second admission policy.
func (heap *managedHeap) reserveWithBatch(batch *managedAllocationBatch, layout, count uint32, logicalBytes uint64) (*managedReservation, error) {
	if batch == nil || batch.heap != heap || batch.finished {
		return heap.reserve(layout, count, logicalBytes, batch)
	}
	reservation, err := batch.reserve(layout, count, logicalBytes)
	if err == nil {
		return reservation, nil
	}
	if err == errManagedCollection && logicalBytes <= managedBatchCredit {
		if refillErr := batch.refill(managedBatchCredit, managedBatchHandles); refillErr == nil {
			if reservation, retryErr := batch.reserve(layout, count, logicalBytes); retryErr == nil {
				return reservation, nil
			}
		}
	}
	// Large/failed worker reservations still use the owner path. The caller
	// publishes the batch before any safepoint, so this preserves the existing
	// failure-atomic path for instructions that cannot be restarted safely.
	reservation, err = heap.reserve(layout, count, logicalBytes)
	if err == nil && batch.transactional {
		reservation.rollbackBatch = batch
	}
	return reservation, err
}

func (reservation *managedReservation) reference() guestRef { return reservation.ref }

// bytes is an unpublished initialization borrow. It must not survive commit,
// abort, a scheduler yield, a host callback or a collection.
func (reservation *managedReservation) bytes() []byte {
	if reservation.done {
		panic("managed reservation is already finished")
	}
	entry := reservation.heap.object(reservation.ref.slot)
	return reservation.heap.pageDirectory.Load().pages[entry.page].data[entry.offset : entry.offset+entry.size]
}

func (reservation *managedReservation) commit() guestRef {
	if reservation.batch != nil {
		return reservation.batch.commit(reservation)
	}
	heap := reservation.heap
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if reservation.done {
		panic("managed reservation committed twice")
	}
	entry := heap.object(reservation.ref.slot)
	entry.occupiedIndex = uint32(len(heap.occupied))
	heap.occupied = append(heap.occupied, reservation.ref.slot)
	if entry.young {
		entry.youngIndex = uint32(len(heap.youngSlots))
		heap.youngSlots = append(heap.youngSlots, reservation.ref.slot)
		heap.youngBytes += entry.logical
	}
	heap.counters.reservedBytes -= entry.logical
	heap.counters.occupiedBytes += entry.logical
	heap.counters.totalAllocatedBytes += entry.logical
	heap.counters.allocations++
	if heap.counters.occupiedBytes >= heap.nextMajorBytes {
		heap.collectionRequested.Store(true)
	}
	heap.unpublished--
	entry.state.Store(managedPublished)
	reservation.done = true
	if !entry.young {
		heap.dirtyRegion(entry, 0, entry.size)
	}
	if reservation.rollbackBatch != nil {
		reservation.rollbackBatch.direct = append(reservation.rollbackBatch.direct, reservation.ref)
	}
	return reservation.ref
}

func (reservation *managedReservation) abort() {
	if reservation.batch != nil {
		reservation.batch.abort(reservation)
		return
	}
	heap := reservation.heap
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if reservation.done {
		return
	}
	entry := heap.object(reservation.ref.slot)
	heap.counters.reservedBytes -= entry.logical
	heap.unpublished--
	heap.freeObject(reservation.ref.slot, false)
	reservation.done = true
}

func (heap *managedHeap) object(slot uint32) *managedObject {
	directory := heap.directory.Load()
	if slot == 0 || slot/managedSegmentSize >= uint32(len(directory.segments)) {
		return nil
	}
	return &directory.segments[slot/managedSegmentSize][slot%managedSegmentSize]
}

func (heap *managedHeap) resolveObject(ref guestRef) (*managedObject, error) {
	entry := heap.object(ref.slot)
	if entry == nil || entry.state.Load() != managedPublished || entry.generation != ref.generation {
		return nil, errManagedReference
	}
	return entry, nil
}

func (heap *managedHeap) read(ref guestRef, offset uint32, dst []byte) error {
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	defer stripe.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	if uint64(offset)+uint64(len(dst)) > uint64(entry.size) {
		return errors.New("managed read outside payload")
	}
	copy(dst, heap.pageDirectory.Load().pages[entry.page].data[entry.offset+offset:entry.offset+offset+uint32(len(dst))])
	return nil
}

// readString copies directly from a validated page span. The returned string
// owns its bytes after the stripe is released, including across relocation.
func (heap *managedHeap) readString(ref guestRef, offset, length uint64) (string, error) {
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	defer stripe.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return "", err
	}
	if offset > uint64(entry.size) || length > uint64(entry.size)-offset {
		return "", errors.New("managed string read outside payload")
	}
	start := uint64(entry.offset) + offset
	return string(heap.pageDirectory.Load().pages[entry.page].data[start : start+length]), nil
}

func (heap *managedHeap) write(ref guestRef, offset uint32, src []byte) error {
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	defer stripe.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	if uint64(offset)+uint64(len(src)) > uint64(entry.size) {
		return errors.New("managed write outside payload")
	}
	copy(heap.pageDirectory.Load().pages[entry.page].data[entry.offset+offset:entry.offset+offset+uint32(len(src))], src)
	heap.dirtyRegion(entry, offset, uint32(len(src)))
	return nil
}

// copy preserves overlapping value assignment without materializing a host
// buffer. Fixed stripe ordering also covers two handles sharing one stripe.
func (heap *managedHeap) copy(destination guestRef, destinationOffset uint32, source guestRef, sourceOffset, size uint32) error {
	first, second := destination.slot%managedStripeCount, source.slot%managedStripeCount
	if first > second {
		first, second = second, first
	}
	heap.stripes[first].Lock()
	defer heap.stripes[first].Unlock()
	if first != second {
		heap.stripes[second].Lock()
		defer heap.stripes[second].Unlock()
	}
	target, err := heap.resolveObject(destination)
	if err != nil {
		return err
	}
	origin, err := heap.resolveObject(source)
	if err != nil {
		return err
	}
	if uint64(destinationOffset)+uint64(size) > uint64(target.size) || uint64(sourceOffset)+uint64(size) > uint64(origin.size) {
		return errors.New("managed copy outside payload")
	}
	pages := heap.pageDirectory.Load().pages
	targetStart, sourceStart := target.offset+destinationOffset, origin.offset+sourceOffset
	copy(pages[target.page].data[targetStart:targetStart+size], pages[origin.page].data[sourceStart:sourceStart+size])
	heap.dirtyRegion(target, destinationOffset, size)
	return nil
}

func (heap *managedHeap) clear(ref guestRef, offset, size uint32) error {
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	defer stripe.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	if uint64(offset)+uint64(size) > uint64(entry.size) {
		return errors.New("managed clear outside payload")
	}
	clear(heap.pageDirectory.Load().pages[entry.page].data[entry.offset+offset : entry.offset+offset+size])
	heap.dirtyRegion(entry, offset, size)
	return nil
}

func (heap *managedHeap) readWord(ref guestRef, offset uint32) (uint64, error) {
	var b [8]byte
	err := heap.read(ref, offset, b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}

func (heap *managedHeap) writeWord(ref guestRef, offset uint32, word uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], word)
	return heap.write(ref, offset, b[:])
}

func (heap *managedHeap) readReference(ref guestRef, offset uint32) (guestRef, error) {
	word, err := heap.readWord(ref, offset)
	return guestRef{slot: uint32(word), generation: uint32(word >> 32)}, err
}

func (heap *managedHeap) writeReference(ref guestRef, offset uint32, child guestRef) error {
	return heap.writeWord(ref, offset, uint64(child.slot)|uint64(child.generation)<<32)
}

// Each old card is queued once until it contains no young references. Its log
// capacity is admitted with its page, so a mutator does not allocate here.
func (heap *managedHeap) dirtyRegion(entry *managedObject, offset, size uint32) {
	if entry.young || size == 0 {
		return
	}
	page := heap.pageDirectory.Load().pages[entry.page]
	first, last := (entry.offset+offset)/managedCardBytes, (entry.offset+offset+size-1)/managedCardBytes
	for card := first; card <= last; card++ {
		if page.cards[card].CompareAndSwap(0, 1) {
			heap.cardsMu.Lock()
			heap.pendingCards = append(heap.pendingCards, managedCard{page: entry.page, index: card})
			heap.cardsMu.Unlock()
		}
	}
}

func (heap *managedHeap) release(ref guestRef) error {
	heap.mu.Lock()
	defer heap.mu.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	heap.counters.reclaimedBytes += entry.logical
	heap.freeObject(ref.slot, true)
	return nil
}

func (heap *managedHeap) freeObject(slot uint32, published bool) {
	entry := heap.object(slot)
	if entry == nil || entry.state.Load() == managedFree {
		return
	}
	ref := guestRef{slot: slot, generation: entry.generation}
	page := heap.pages[entry.page]
	if !entry.young {
		if page.kind == managedOldPage {
			index := entry.offset / page.class
			page.entries[index] = 0
			if len(page.free) == 0 {
				class := bits.TrailingZeros32(page.class) - 3
				heap.oldAvailable[class] = append(heap.oldAvailable[class], entry.page)
			}
			page.free = append(page.free, index)
			page.used -= page.class
		} else {
			page.entries[0], page.used = 0, 0
		}
	}
	if published {
		heap.counters.occupiedBytes -= entry.logical
		index := entry.occupiedIndex
		last := heap.occupied[len(heap.occupied)-1]
		heap.occupied[index] = last
		heap.object(last).occupiedIndex = index
		heap.occupied = heap.occupied[:len(heap.occupied)-1]
		if entry.young {
			heap.youngBytes -= entry.logical
			index = entry.youngIndex
			last = heap.youngSlots[len(heap.youngSlots)-1]
			heap.youngSlots[index] = last
			heap.object(last).youngIndex = index
			heap.youngSlots = heap.youngSlots[:len(heap.youngSlots)-1]
		}
	}
	entry.state.Store(managedFree)
	entry.logical = 0
	if published && heap.onReclaim != nil {
		heap.onReclaim(ref)
	}
	if entry.generation != math.MaxUint32 {
		heap.freeSlots = append(heap.freeSlots, slot)
	}
}

func (heap *managedHeap) stats() managedHeapStats {
	heap.mu.Lock()
	defer heap.mu.Unlock()
	return heap.counters
}

// managedControlReservation accounts runtime-owned storage that has no guest payload
// record (frame activation, task/control records and owner queues). It uses
// the same occupied/reserved ledger as page payload reservations so a control
// allocation cannot pass the guest budget while a payload reservation is in
// flight, and it is released explicitly by its owner.
type managedControlReservation struct {
	heap  *managedHeap
	bytes uint64
}

func (heap *managedHeap) reserveControl(logical uint64, batches ...*managedAllocationBatch) (managedControlReservation, error) {
	if heap == nil || logical == 0 {
		return managedControlReservation{}, nil
	}
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if heap.closed || logical > math.MaxInt64 {
		return managedControlReservation{}, errManagedLimit
	}
	transfer, err := heap.admitGuestBytes(logical, batches...)
	if err != nil {
		return managedControlReservation{}, errManagedCollection
	}
	defer transfer.rollback()
	heap.counters.reservedBytes += logical
	transfer.bytes = 0
	return managedControlReservation{heap: heap, bytes: logical}, nil
}

func (reservation *managedControlReservation) commit() {
	if reservation.heap == nil {
		return
	}
	heap := reservation.heap
	heap.mu.Lock()
	heap.counters.reservedBytes -= reservation.bytes
	heap.counters.occupiedBytes += reservation.bytes
	heap.counters.totalAllocatedBytes += reservation.bytes
	heap.counters.allocations++
	heap.controlBytes += reservation.bytes
	if heap.counters.occupiedBytes >= heap.nextMajorBytes {
		heap.collectionRequested.Store(true)
	}
	heap.mu.Unlock()
	reservation.heap = nil
}

func (reservation *managedControlReservation) abort() {
	if reservation.heap == nil {
		return
	}
	heap := reservation.heap
	heap.mu.Lock()
	heap.counters.reservedBytes -= reservation.bytes
	heap.mu.Unlock()
	reservation.heap = nil
}

func (heap *managedHeap) releaseControl(logical uint64) {
	if heap == nil || logical == 0 {
		return
	}
	heap.mu.Lock()
	if logical > heap.controlBytes {
		logical = heap.controlBytes
	}
	heap.controlBytes -= logical
	heap.counters.occupiedBytes -= logical
	heap.counters.reclaimedBytes += logical
	heap.mu.Unlock()
}

func (heap *managedHeap) close() {
	heap.mu.Lock()
	defer heap.mu.Unlock()
	heap.closed = true
	for _, page := range heap.pages {
		page.data, page.entries, page.free, page.cards = nil, nil, nil, nil
	}
	heap.segments, heap.pages, heap.layouts = nil, nil, nil
	heap.layoutOwners = nil
	heap.directory.Store(&managedDirectory{})
	heap.pageDirectory.Store(&managedPageDirectory{})
	heap.layoutDirectory.Store(&managedLayoutDirectory{})
	heap.freePages, heap.freeSlots, heap.occupied, heap.youngSlots, heap.nurseryPages = nil, nil, nil, nil, nil
	heap.oldAvailable = [14][]uint32{}
	heap.pendingCards, heap.queue = nil, nil
	heap.nurseryCapacity = 0
	heap.youngBytes = 0
	heap.controlBytes = 0
	heap.counters.occupiedBytes, heap.counters.reservedBytes, heap.counters.residentPageBytes = 0, 0, 0
}
