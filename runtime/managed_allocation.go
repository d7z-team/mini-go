package runtime

import (
	"errors"
	"math"
	"math/bits"
	"unsafe"
)

const (
	managedBatchCredit  uint64 = 64 << 10
	managedBatchHandles uint32 = 128
)

// A batch owns real nursery space, directory tickets, logical credits and a
// bounded card log. reserve/commit never acquire the heap owner lock. The
// scheduler returns the batch before publishing a safepoint to the collector.
type managedAllocationBatch struct {
	heap                *managedHeap
	page                uint32
	pages               []uint32
	reservations        [][]managedReservation
	reservationCapacity uint32
	cards               []managedCard
	credit              uint64
	reserved            uint64
	committed           uint64
	totalAllocatedBytes uint64
	allocations         uint64
	used                uint32
	classes             uint16
	finished            bool
	failure             error
	resident            uint64
	// direct contains owner reservations made after this lease could not
	// satisfy an allocation. It is used by short-lived transactional map
	// batches so their failure path still owns those publications.
	direct        []guestRef
	transactional bool
}

func (batch *managedAllocationBatch) publishedRefs() []guestRef {
	if batch == nil {
		return nil
	}
	refs := append([]guestRef(nil), batch.direct...)
	for _, chunk := range batch.reservations {
		for index := range chunk {
			reservation := &chunk[index]
			if !reservation.done {
				continue
			}
			entry := batch.heap.object(reservation.ref.slot)
			if entry != nil && entry.state.Load() == managedPublished {
				refs = append(refs, reservation.ref)
			}
		}
	}
	return refs
}

func (heap *managedHeap) acquireBatch(credit uint64, handles uint32) (*managedAllocationBatch, error) {
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if heap.closed || credit == 0 || handles == 0 || handles > managedSegmentSize {
		return nil, errors.New("invalid managed allocation batch")
	}
	used := heap.counters.occupiedBytes + heap.counters.reservedBytes
	if credit > math.MaxInt64-used || heap.maxGuestBytes != 0 && credit > heap.maxGuestBytes-min(heap.maxGuestBytes, used) {
		return nil, errManagedCollection
	}
	// Free tickets at generation exhaustion retire instead of wrapping. This
	// pass runs at refill, never in a worker allocation or collection.
	for len(heap.freeSlots) != 0 && heap.object(heap.freeSlots[len(heap.freeSlots)-1]).generation == math.MaxUint32 {
		heap.freeSlots = heap.freeSlots[:len(heap.freeSlots)-1]
	}
	fresh := max(0, int(handles)-len(heap.freeSlots))
	if uint64(heap.nextSlot)+uint64(fresh) > math.MaxUint32 {
		return nil, errManagedLimit
	}
	for uint64(len(heap.segments))*uint64(managedSegmentSize) < uint64(heap.nextSlot)+uint64(fresh) {
		if err := heap.growDirectory(); err != nil {
			return nil, err
		}
	}
	pageID, err := heap.nurseryPage(managedPageBytes / 4)
	if err != nil {
		return nil, err
	}
	const cardEntries = 64
	resident := uint64(unsafe.Sizeof(managedAllocationBatch{})) + uint64(handles)*uint64(unsafe.Sizeof(managedReservation{})) + cardEntries*uint64(unsafe.Sizeof(managedCard{}))
	if !heap.admitResident(resident) {
		return nil, errManagedLimit
	}
	batch := &managedAllocationBatch{
		heap: heap, page: pageID, credit: credit, resident: resident,
		pages: []uint32{pageID}, reservations: [][]managedReservation{make([]managedReservation, handles)},
		reservationCapacity: handles, cards: make([]managedCard, 0, cardEntries),
	}
	// Observed allocation classes are prepared at refill. Workers only record
	// their local mask; they never request promotion pages per allocation.
	for classes := heap.batchClasses; classes != 0; classes &= classes - 1 {
		index := bits.TrailingZeros16(classes)
		if len(heap.oldAvailable[index]) == 0 {
			_, _ = heap.newPage(managedPageBytes, managedOldPage, uint32(8)<<index, false)
		}
	}
	for index := range batch.reservations[0] {
		var slot uint32
		if len(heap.freeSlots) > 0 {
			slot = heap.freeSlots[len(heap.freeSlots)-1]
			heap.freeSlots = heap.freeSlots[:len(heap.freeSlots)-1]
		} else {
			slot, heap.nextSlot = heap.nextSlot, heap.nextSlot+1
		}
		batch.reservations[0][index] = managedReservation{heap: heap, batch: batch, ref: guestRef{slot: slot}}
	}
	heap.pages[pageID].leased = true
	heap.counters.reservedBytes += credit
	heap.activeBatches++
	return batch, nil
}

// refill extends a live lease only at its slow boundary. The worker keeps its
// existing unpublished reservations and switches to a newly leased nursery
// page; collection remains excluded until the eventual flush.
func (batch *managedAllocationBatch) refill(credit uint64, handles uint32) error {
	if batch == nil || batch.finished || credit == 0 || handles == 0 {
		return errManagedCollection
	}
	heap := batch.heap
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if batch.finished || heap.closed {
		return errManagedCollection
	}
	used := heap.counters.occupiedBytes + heap.counters.reservedBytes
	if credit > math.MaxInt64-used || heap.maxGuestBytes != 0 && credit > heap.maxGuestBytes-min(heap.maxGuestBytes, used) {
		return errManagedCollection
	}
	for len(heap.freeSlots) != 0 && heap.object(heap.freeSlots[len(heap.freeSlots)-1]).generation == math.MaxUint32 {
		heap.freeSlots = heap.freeSlots[:len(heap.freeSlots)-1]
	}
	fresh := max(0, int(handles)-len(heap.freeSlots))
	if uint64(heap.nextSlot)+uint64(fresh) > math.MaxUint32 {
		return errManagedLimit
	}
	for uint64(len(heap.segments))*uint64(managedSegmentSize) < uint64(heap.nextSlot)+uint64(fresh) {
		if err := heap.growDirectory(); err != nil {
			return err
		}
	}
	pageID, err := heap.nurseryPage(managedPageBytes / 4)
	if err != nil {
		return err
	}
	const cardEntries = 64
	resident := uint64(handles)*uint64(unsafe.Sizeof(managedReservation{})) + cardEntries*uint64(unsafe.Sizeof(managedCard{}))
	if !heap.admitResident(resident) {
		return errManagedLimit
	}
	reservations := make([]managedReservation, handles)
	for index := range reservations {
		var slot uint32
		if len(heap.freeSlots) != 0 {
			slot = heap.freeSlots[len(heap.freeSlots)-1]
			heap.freeSlots = heap.freeSlots[:len(heap.freeSlots)-1]
		} else {
			slot, heap.nextSlot = heap.nextSlot, heap.nextSlot+1
		}
		reservations[index] = managedReservation{heap: heap, batch: batch, ref: guestRef{slot: slot}}
	}
	batch.reservations = append(batch.reservations, reservations)
	batch.reservationCapacity += handles
	if cap(batch.cards)-len(batch.cards) < cardEntries {
		cards := make([]managedCard, len(batch.cards), len(batch.cards)+cardEntries)
		copy(cards, batch.cards)
		batch.cards = cards
	}
	heap.pages[batch.page].leased = true
	heap.pages[pageID].leased = true
	batch.pages = append(batch.pages, pageID)
	batch.page = pageID
	batch.credit += credit
	batch.resident += resident
	heap.counters.reservedBytes += credit
	return nil
}

func (batch *managedAllocationBatch) reserve(layout, count uint32, logical uint64) (*managedReservation, error) {
	if batch.finished {
		return nil, errors.New("managed allocation batch already returned")
	}
	layouts := batch.heap.layoutDirectory.Load().layouts
	if layout == 0 || int(layout) >= len(layouts) {
		return nil, errors.New("invalid managed allocation layout")
	}
	physical := uint64(layouts[layout].stride) * uint64(count)
	if physical > uint64(managedPageBytes/4) {
		return nil, errManagedCollection
	}
	size := uint32(max(uint64(8), (physical+7)&^7))
	page := batch.heap.pageDirectory.Load().pages[batch.page]
	if batch.used == batch.reservationCapacity || size > uint32(len(page.data))-page.used || logical > batch.credit-batch.reserved-batch.committed {
		return nil, errManagedCollection
	}
	index := batch.used
	var reservation *managedReservation
	for _, chunk := range batch.reservations {
		if index < uint32(len(chunk)) {
			reservation = &chunk[index]
			break
		}
		index -= uint32(len(chunk))
	}
	if reservation == nil {
		return nil, errManagedCollection
	}
	batch.used++
	entry := batch.heap.object(reservation.ref.slot)
	entry.generation++
	entry.layout, entry.count, entry.page, entry.offset, entry.size = layout, count, batch.page, page.used, size
	entry.logical, entry.age, entry.young, entry.marked = logical, 0, true, 0
	entry.state.Store(managedReserved)
	reservation.ref.generation = entry.generation
	clear(page.data[page.used : page.used+size])
	page.used += size
	batch.reserved += logical
	batch.classes |= 1 << (max(3, bits.Len32(size-1)) - 3)
	return reservation, nil
}

func (batch *managedAllocationBatch) commit(reservation *managedReservation) guestRef {
	if batch.finished || reservation.done {
		panic("managed reservation committed after completion")
	}
	entry := batch.heap.object(reservation.ref.slot)
	batch.reserved -= entry.logical
	batch.committed += entry.logical
	batch.totalAllocatedBytes += entry.logical
	batch.allocations++
	reservation.done = true
	entry.state.Store(managedPublished)
	return reservation.ref
}

func (batch *managedAllocationBatch) abort(reservation *managedReservation) {
	if reservation.done {
		return
	}
	entry := batch.heap.object(reservation.ref.slot)
	batch.reserved -= entry.logical
	entry.state.Store(managedFree)
	entry.logical = 0
	reservation.done = true
}

// abortUnfinished is used when an instruction is being retried or fails after
// publishing some values through the batch. Committed records remain visible;
// only unpublished reservations are rolled back before the lease is flushed.
func (batch *managedAllocationBatch) abortUnfinished() {
	if batch == nil || batch.finished {
		return
	}
	heap := batch.heap
	heap.mu.Lock()
	defer heap.mu.Unlock()
	for _, chunk := range batch.reservations {
		for index := range chunk {
			if !chunk[index].done {
				entry := heap.object(chunk[index].ref.slot)
				if entry != nil && entry.state.Load() == managedReserved {
					batch.reserved -= entry.logical
					entry.logical = 0
					entry.state.Store(managedFree)
				}
				chunk[index].done = true
			}
		}
	}
}

// write reserves enough log capacity before touching an old region. A full
// log asks the worker to return its batch; it never drops a remembered edge.
func (batch *managedAllocationBatch) write(ref guestRef, offset uint32, data []byte) error {
	if batch.finished {
		return errors.New("managed allocation batch already returned")
	}
	heap := batch.heap
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	defer stripe.Unlock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	if uint64(offset)+uint64(len(data)) > uint64(entry.size) {
		return errors.New("managed write outside payload")
	}
	page := heap.pageDirectory.Load().pages[entry.page]
	var first, last uint32
	if !entry.young && len(data) > 0 {
		first, last = (entry.offset+offset)/managedCardBytes, (entry.offset+offset+uint32(len(data))-1)/managedCardBytes
		needed := 0
		for card := first; card <= last; card++ {
			if page.cards[card].Load() == 0 {
				needed++
			}
		}
		if needed > cap(batch.cards)-len(batch.cards) {
			return errManagedCollection
		}
	}
	copy(page.data[entry.offset+offset:entry.offset+offset+uint32(len(data))], data)
	if !entry.young && len(data) > 0 {
		for card := first; card <= last; card++ {
			if page.cards[card].CompareAndSwap(0, 1) {
				batch.cards = append(batch.cards, managedCard{page: entry.page, index: card})
			}
		}
	}
	return nil
}

func (batch *managedAllocationBatch) flush() error {
	heap := batch.heap
	heap.mu.Lock()
	defer heap.mu.Unlock()
	if batch.finished {
		return nil
	}
	if batch.reserved != 0 {
		return errors.New("managed allocation batch contains unfinished reservations")
	}
	// Zero-cost reservations also need a terminal publication decision.
	var used uint32
	for _, chunk := range batch.reservations {
		for index := range chunk {
			if used == batch.used {
				break
			}
			if !chunk[index].done {
				return errors.New("managed allocation batch contains unfinished reservations")
			}
			used++
		}
	}
	used = 0
	for _, chunk := range batch.reservations {
		for index := range chunk {
			reservation := &chunk[index]
			entry := heap.object(reservation.ref.slot)
			if used < batch.used && entry.state.Load() == managedPublished {
				entry.occupiedIndex, entry.youngIndex = uint32(len(heap.occupied)), uint32(len(heap.youngSlots))
				heap.occupied = append(heap.occupied, reservation.ref.slot)
				heap.youngSlots = append(heap.youngSlots, reservation.ref.slot)
			} else if entry.generation != math.MaxUint32 {
				heap.freeSlots = append(heap.freeSlots, reservation.ref.slot)
			}
			used++
		}
	}
	heap.cardsMu.Lock()
	heap.pendingCards = append(heap.pendingCards, batch.cards...)
	heap.cardsMu.Unlock()
	heap.counters.reservedBytes -= batch.credit
	heap.counters.occupiedBytes += batch.committed
	heap.youngBytes += batch.committed
	heap.counters.totalAllocatedBytes += batch.totalAllocatedBytes
	heap.counters.allocations += batch.allocations
	heap.counters.reclaimedBytes += batch.totalAllocatedBytes - batch.committed
	if heap.counters.occupiedBytes >= heap.nextMajorBytes {
		heap.collectionRequested.Store(true)
	}
	heap.batchClasses |= batch.classes
	heap.counters.residentPageBytes -= batch.resident
	for _, pageID := range batch.pages {
		heap.pages[pageID].leased = false
	}
	heap.activeBatches--
	batch.finished = true
	batch.reservations, batch.cards = nil, nil
	batch.reservationCapacity = 0
	return nil
}
