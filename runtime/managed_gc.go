package runtime

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
	"slices"
	"sync"
	"time"
)

var (
	errManagedCollectorScratch = errors.New("managed collector scratch is not pre-admitted")
	errManagedCollectorLayout  = errors.New("managed collector found an invalid payload layout")
	errManagedCollectorBusy    = errors.New("managed collector requires flushed allocations")
)

const managedCopyPoison byte = 0xdd

const (
	managedMaxCollectorWorkers   = 4
	managedParallelMarkMinSlots  = 256
	managedParallelSweepMinSlots = 256
	managedParallelSweepChunk    = 128
)

// Keep the collector buildable with the page-core file set used by the
// focused tests. These values are the stable dynamic representation tags
// declared by managed_value.go.
const (
	managedCollectorValueRef = 4
	managedCollectorValueBox = 5
)

// collect runs at the stopped-mutator boundary. The collector deliberately
// does not grow any work list or page while it is tracing: reserve admits the
// directory, card log, and nursery pair before a mutator can publish an
// object. Handles remain stable while their payload is copied.
func (heap *managedHeap) collect(roots []guestRef, major bool) (managedCollection, error) {
	return heap.collectFromRoots(func(visit func(guestRef)) {
		for _, root := range roots {
			visit(root)
		}
	}, major)
}

func (heap *managedHeap) collectFromRoots(visitRoots func(func(guestRef)), major bool) (managedCollection, error) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	if heap.closed {
		return managedCollection{}, errors.New("managed heap is closed")
	}
	if heap.unpublished != 0 || heap.activeBatches != 0 {
		return managedCollection{}, errManagedCollectorBusy
	}
	// A root is only an input to marking; each live slot is enqueued at most
	// once per epoch.  Duplicate roots therefore do not consume scratch.
	if cap(heap.queue) < len(heap.occupied) {
		return managedCollection{}, errManagedCollectorScratch
	}
	started := time.Now()
	defer func() { heap.counters.gcNanos += uint64(time.Since(started).Nanoseconds()) }()

	epoch := heap.nextCollectionEpoch()
	collection := managedCollection{reachableBytes: heap.controlBytes}
	heap.queue = heap.queue[:0]
	var rootError error
	visitRoots(func(root guestRef) {
		if rootError != nil {
			return
		}
		if !root.valid() {
			if root.slot != 0 || root.generation != 0 {
				rootError = errManagedReference
			}
			return
		}
		collection.roots++
		rootError = heap.markReference(root, epoch, major, &collection)
	})
	if rootError != nil {
		return managedCollection{}, rootError
	}
	var err error
	if major && heap.collectorWorkers > 1 && len(heap.queue) >= managedParallelMarkMinSlots {
		err = heap.drainQueueParallel(epoch, &collection)
	} else {
		err = heap.drainQueue(epoch, major, &collection)
	}
	if err != nil {
		return managedCollection{}, err
	}
	if !major {
		if err := heap.tracePendingCards(epoch, &collection); err != nil {
			return managedCollection{}, err
		}
		// Card tracing can discover a young object whose payload contains the
		// next edge.  Drain again after cards; otherwise only the first edge of
		// an old -> young -> young chain survives a minor collection.
		if err := heap.drainQueue(epoch, major, &collection); err != nil {
			return managedCollection{}, err
		}
	}

	if major {
		if err := heap.sweepOldParallel(epoch, &collection); err != nil {
			return managedCollection{}, err
		}
		heap.reclaimEmptyPages()
	}
	if err := heap.copyNursery(epoch, &collection); err != nil {
		return managedCollection{}, err
	}
	if major {
		heap.resetCards()
		if err := heap.rebuildYoungCards(); err != nil {
			return managedCollection{}, err
		}
	} else if err := heap.refreshPendingCards(); err != nil {
		return managedCollection{}, err
	}

	// Minor collection does not mark old objects. They remain occupied by
	// definition, while dead young objects have already been removed.
	if !major {
		collection.reachableBytes = heap.counters.occupiedBytes
	}
	heap.counters.copiedBytes += collection.copiedBytes
	heap.counters.promotedBytes += collection.promotedBytes
	heap.counters.reclaimedBytes += collection.reclaimedBytes
	heap.counters.scannedRoots += collection.roots
	heap.counters.scannedCards += collection.cards
	heap.counters.scannedReferences += collection.references
	if major {
		heap.counters.majorCollections++
		heap.counters.lastMajorReachableBytes = collection.reachableBytes
		heap.nextMajorBytes = collection.reachableBytes + max(collection.reachableBytes, managedMajorGrowthBytes)
		// Only major traverses the descriptor directory. Live records and
		// explicit construction owners keep their immutable physical shape.
		for _, slot := range heap.occupied {
			heap.layoutOwners[heap.object(slot).layout].marked = epoch
		}
		for id := 1; id < len(heap.layouts); id++ {
			owner := heap.layoutOwners[id]
			if owner.pins != 0 || owner.marked == epoch {
				if heap.onType != nil && heap.layouts[id].typeID != 0 {
					heap.onType(heap.layouts[id].typeID, epoch)
				}
				continue
			}
			layout := heap.layouts[id]
			heap.counters.residentPageBytes -= uint64(len(layout.references)+len(layout.dynamics)+len(layout.types)) * 4
			heap.layouts[id] = managedLayout{}
		}
		if heap.onMajor != nil {
			heap.counters.residentPageBytes -= min(heap.counters.residentPageBytes, heap.onMajor(epoch))
		}
	} else {
		heap.counters.minorCollections++
	}
	heap.collectionRequested.Store(false)
	return collection, nil
}

func (heap *managedHeap) nextCollectionEpoch() uint64 {
	heap.epoch++
	if heap.epoch != 0 {
		return heap.epoch
	}
	heap.epoch = 1
	for _, slot := range heap.occupied {
		if entry := heap.object(slot); entry != nil {
			entry.marked = 0
		}
	}
	return heap.epoch
}

func (heap *managedHeap) enqueue(slot uint32) error {
	if len(heap.queue) == cap(heap.queue) {
		return errManagedCollectorScratch
	}
	heap.queue = append(heap.queue, slot)
	return nil
}

func (heap *managedHeap) drainQueue(epoch uint64, major bool, collection *managedCollection) error {
	for cursor := 0; cursor < len(heap.queue); cursor++ {
		entry := heap.object(heap.queue[cursor])
		if entry == nil || entry.state.Load() != managedPublished || entry.marked != epoch {
			return errManagedReference
		}
		// A minor collection traces young payloads. Old payloads are traced
		// through remembered cards; walking the complete old graph here would
		// recreate the global-scan bottleneck.
		if major || entry.young {
			if err := heap.traceObject(entry, epoch, major, 0, math.MaxUint32, collection); err != nil {
				return err
			}
		}
	}
	return nil
}

// drainQueueParallel is the major marker. The owner keeps heap.mu held, so
// payloads cannot move while workers trace them. A fixed mutex protects only
// queue cursor/append; marking itself is striped by guest slot and each
// worker accumulates counters locally, avoiding a global mark lock and any
// collector allocation.
func (heap *managedHeap) drainQueueParallel(epoch uint64, collection *managedCollection) error {
	if len(heap.queue) == 0 {
		return nil
	}
	workers := min(heap.collectorWorkers, managedMaxCollectorWorkers, len(heap.queue))
	if workers < 2 {
		return heap.drainQueue(epoch, true, collection)
	}
	var queueMu sync.Mutex
	cursor := 0
	var firstErr error
	var errMu sync.Mutex
	var group sync.WaitGroup
	group.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer group.Done()
			local := managedCollection{}
			for {
				errMu.Lock()
				failed := firstErr != nil
				errMu.Unlock()
				queueMu.Lock()
				if failed || cursor >= len(heap.queue) {
					queueMu.Unlock()
					break
				}
				slot := heap.queue[cursor]
				cursor++
				queueMu.Unlock()
				entry := heap.object(slot)
				if entry == nil || entry.state.Load() != managedPublished || entry.marked != epoch {
					errMu.Lock()
					if firstErr == nil {
						firstErr = errManagedReference
					}
					errMu.Unlock()
					break
				}
				if err := heap.traceObjectParallel(entry, epoch, &local, &queueMu); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					break
				}
			}
			queueMu.Lock()
			collection.reachableBytes += local.reachableBytes
			collection.references += local.references
			queueMu.Unlock()
		}()
	}
	group.Wait()
	return firstErr
}

func (heap *managedHeap) markReference(ref guestRef, epoch uint64, major bool, collection *managedCollection) error {
	if !ref.valid() {
		if ref.slot != 0 || ref.generation != 0 {
			return errManagedReference
		}
		return nil
	}
	entry, err := heap.resolveObject(ref)
	if err != nil {
		return err
	}
	if !major && !entry.young {
		return nil
	}
	if entry.marked == epoch {
		return nil
	}
	entry.marked = epoch
	collection.reachableBytes += entry.logical
	return heap.enqueue(ref.slot)
}

func (heap *managedHeap) markReferenceParallel(ref guestRef, epoch uint64, collection *managedCollection, queueMu *sync.Mutex) error {
	if !ref.valid() {
		if ref.slot != 0 || ref.generation != 0 {
			return errManagedReference
		}
		return nil
	}
	stripe := &heap.stripes[ref.slot%managedStripeCount]
	stripe.Lock()
	entry, err := heap.resolveObject(ref)
	if err != nil {
		stripe.Unlock()
		return err
	}
	if entry.marked == epoch {
		stripe.Unlock()
		return nil
	}
	entry.marked = epoch
	logical := entry.logical
	stripe.Unlock()
	collection.reachableBytes += logical
	queueMu.Lock()
	err = heap.enqueue(ref.slot)
	queueMu.Unlock()
	return err
}

func managedRangesOverlap(start, size, rangeStart, rangeEnd uint32) bool {
	if size == 0 || rangeStart >= rangeEnd {
		return false
	}
	end := start + size
	return start < rangeEnd && rangeStart < end
}

func (heap *managedHeap) traceObject(entry *managedObject, epoch uint64, major bool, rangeStart, rangeEnd uint32, collection *managedCollection) error {
	return heap.traceObjectWithQueue(entry, epoch, major, rangeStart, rangeEnd, collection, nil)
}

func (heap *managedHeap) traceObjectParallel(entry *managedObject, epoch uint64, collection *managedCollection, queueMu *sync.Mutex) error {
	return heap.traceObjectWithQueue(entry, epoch, true, 0, math.MaxUint32, collection, queueMu)
}

func (heap *managedHeap) traceObjectWithQueue(entry *managedObject, epoch uint64, major bool, rangeStart, rangeEnd uint32, collection *managedCollection, queueMu *sync.Mutex) error {
	if entry.layout == 0 || int(entry.layout) >= len(heap.layouts) {
		return errManagedCollectorLayout
	}
	layout := heap.layouts[entry.layout]
	if layout.stride == 0 {
		return errManagedCollectorLayout
	}
	page := heap.pages[entry.page]
	if page == nil || uint64(entry.offset)+uint64(entry.count)*uint64(layout.stride) > uint64(len(page.data)) {
		return errManagedCollectorLayout
	}
	first, last := managedElementRange(entry, layout, rangeStart, rangeEnd)
	for element := first; element < last; element++ {
		base := entry.offset + element*layout.stride
		if major && heap.onType != nil {
			for _, offset := range layout.types {
				heap.onType(binary.LittleEndian.Uint32(page.data[base+offset:]), epoch)
			}
			for _, offset := range layout.dynamics {
				heap.onType(binary.LittleEndian.Uint32(page.data[base+offset:]), epoch)
			}
		}
		referenceStart, _ := slices.BinarySearch(layout.references, uint32(max(0, int64(rangeStart)-int64(base)-7)))
		for _, offset := range layout.references[referenceStart:] {
			if offset+8 > layout.stride || !managedRangesOverlap(base+offset, 8, rangeStart, rangeEnd) {
				if rangeEnd != math.MaxUint32 && base+offset >= rangeEnd {
					break
				}
				continue
			}
			word := binary.LittleEndian.Uint64(page.data[base+offset:])
			child := guestRef{slot: uint32(word), generation: uint32(word >> 32)}
			if child.slot == 0 && child.generation == 0 {
				continue
			}
			collection.references++
			var err error
			if queueMu != nil {
				err = heap.markReferenceParallel(child, epoch, collection, queueMu)
			} else {
				err = heap.markReference(child, epoch, major, collection)
			}
			if err != nil {
				return err
			}
		}
		dynamicStart, _ := slices.BinarySearch(layout.dynamics, uint32(max(0, int64(rangeStart)-int64(base)-15)))
		for _, dynamic := range layout.dynamics[dynamicStart:] {
			if dynamic >= layout.stride || rangeEnd != math.MaxUint32 && base+dynamic >= rangeEnd {
				if rangeEnd != math.MaxUint32 && base+dynamic >= rangeEnd {
					break
				}
				continue
			}
			if dynamic+24 > layout.stride || !managedRangesOverlap(base+dynamic+4, 12, rangeStart, rangeEnd) {
				continue
			}
			representation := binary.LittleEndian.Uint32(page.data[base+dynamic+4:])
			if representation != managedCollectorValueRef && representation != managedCollectorValueBox {
				continue
			}
			word := binary.LittleEndian.Uint64(page.data[base+dynamic+8:])
			child := guestRef{slot: uint32(word), generation: uint32(word >> 32)}
			if child.slot == 0 && child.generation == 0 {
				continue
			}
			collection.references++
			var err error
			if queueMu != nil {
				err = heap.markReferenceParallel(child, epoch, collection, queueMu)
			} else {
				err = heap.markReference(child, epoch, major, collection)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func managedElementRange(entry *managedObject, layout managedLayout, rangeStart, rangeEnd uint32) (uint32, uint32) {
	if rangeEnd == math.MaxUint32 {
		return 0, entry.count
	}
	if rangeStart <= entry.offset {
		rangeStart = entry.offset
	}
	if rangeEnd <= entry.offset {
		return 0, 0
	}
	first := (rangeStart - entry.offset) / layout.stride
	if first > entry.count {
		first = entry.count
	}
	lastBytes := rangeEnd - entry.offset
	last := (lastBytes + layout.stride - 1) / layout.stride
	if last > entry.count {
		last = entry.count
	}
	return first, last
}

func (heap *managedHeap) tracePendingCards(epoch uint64, collection *managedCollection) error {
	write := 0
	for _, card := range heap.pendingCards {
		if card.page >= uint32(len(heap.pages)) || heap.pages[card.page] == nil {
			return errManagedCollectorLayout
		}
		page := heap.pages[card.page]
		if card.index >= uint32(len(page.cards)) {
			return errManagedCollectorLayout
		}
		collection.cards++
		start := card.index * managedCardBytes
		end := start + managedCardBytes
		if end > uint32(len(page.data)) {
			end = uint32(len(page.data))
		}
		first, last := managedCardEntryRange(page, start, end)
		for index := first; index < last; index++ {
			slot := page.entries[index]
			if slot == 0 {
				continue
			}
			entry := heap.object(slot)
			if entry == nil || entry.state.Load() != managedPublished || entry.page != card.page || !managedRangesOverlap(entry.offset, entry.size, start, end) {
				continue
			}
			if err := heap.traceObject(entry, epoch, false, start, end, collection); err != nil {
				return err
			}
		}
		page.cards[card.index].Store(1)
		heap.pendingCards[write] = card
		write++
	}
	heap.pendingCards = heap.pendingCards[:write]
	return nil
}

func managedCardEntryRange(page *managedPage, start, end uint32) (uint32, uint32) {
	if start >= end || len(page.entries) == 0 {
		return 0, 0
	}
	if page.kind == managedLargePage {
		if start >= page.used {
			return 0, 0
		}
		return 0, 1
	}
	if page.kind != managedOldPage || page.class == 0 {
		return 0, 0
	}
	first := start / page.class
	last := (end + page.class - 1) / page.class
	if first > uint32(len(page.entries)) {
		first = uint32(len(page.entries))
	}
	if last > uint32(len(page.entries)) {
		last = uint32(len(page.entries))
	}
	return first, last
}

func (heap *managedHeap) copyNursery(epoch uint64, collection *managedCollection) error {
	for _, fromID := range heap.nurseryPages {
		if fromID >= uint32(len(heap.pages)) || heap.pages[fromID] == nil {
			return errManagedCollectorLayout
		}
		from := heap.pages[fromID]
		toID := from.partner
		if toID >= uint32(len(heap.pages)) || heap.pages[toID] == nil {
			return errManagedCollectorLayout
		}
		to := heap.pages[toID]
		to.kind, to.used = managedYoungPage, 0
	}
	for cursor := 0; cursor < len(heap.youngSlots); cursor++ {
		slot := heap.youngSlots[cursor]
		entry := heap.object(slot)
		if entry == nil || entry.state.Load() != managedPublished || entry.marked != epoch {
			continue
		}
		from := heap.pages[entry.page]
		if from == nil || from.kind != managedYoungPage {
			return errManagedCollectorLayout
		}
		toID := from.partner
		if toID >= uint32(len(heap.pages)) || heap.pages[toID] == nil {
			return errManagedCollectorLayout
		}
		to := heap.pages[toID]
		if entry.age >= 1 {
			if err := heap.promoteObject(entry, slot, collection); err == nil {
				continue
			}
		}
		aligned := (to.used + 7) &^ 7
		if uint64(aligned)+uint64(entry.size) > uint64(len(to.data)) || uint64(entry.offset)+uint64(entry.size) > uint64(len(from.data)) {
			return errManagedCollectorScratch
		}
		copy(to.data[aligned:aligned+entry.size], from.data[entry.offset:entry.offset+entry.size])
		entry.page, entry.offset, entry.age, entry.young = toID, aligned, entry.age+1, true
		to.used = aligned + entry.size
		collection.copiedBytes += uint64(entry.size)
	}
	for index, fromID := range heap.nurseryPages {
		from := heap.pages[fromID]
		toID := from.partner
		to := heap.pages[toID]
		heap.nurseryPages[index] = toID
		to.partner, from.partner = fromID, toID
		from.kind, from.used = managedSparePage, 0
		if heap.poisonCollected {
			for offset := range from.data {
				from.data[offset] = managedCopyPoison
			}
		}
	}
	// Remove unreachable young objects after all survivors have moved. The
	// swap-delete vector is deliberately traversed without allocating.
	for index := 0; index < len(heap.youngSlots); {
		entry := heap.object(heap.youngSlots[index])
		if entry != nil && entry.state.Load() == managedPublished && entry.young && entry.marked == epoch {
			index++
			continue
		}
		slot := heap.youngSlots[index]
		entry = heap.object(slot)
		if entry != nil && entry.state.Load() == managedPublished && !entry.young {
			heap.removeYoungSlot(slot)
			continue
		}
		if entry != nil && entry.state.Load() == managedPublished {
			collection.reclaimedBytes += entry.logical
			heap.freeObject(slot, true)
		} else if entry != nil && entry.state.Load() != managedFree {
			heap.freeObject(slot, false)
		}
	}
	return nil
}

func (heap *managedHeap) removeYoungSlot(slot uint32) {
	entry := heap.object(slot)
	if entry == nil || entry.youngIndex >= uint32(len(heap.youngSlots)) || heap.youngSlots[entry.youngIndex] != slot {
		return
	}
	index := entry.youngIndex
	last := heap.youngSlots[len(heap.youngSlots)-1]
	heap.youngSlots[index] = last
	heap.object(last).youngIndex = index
	heap.youngSlots = heap.youngSlots[:len(heap.youngSlots)-1]
}

func (heap *managedHeap) promoteObject(entry *managedObject, slot uint32, collection *managedCollection) error {
	classIndex := -1
	for index, pages := range heap.oldAvailable {
		class := uint32(1) << uint(index+3)
		if class >= entry.size && len(pages) != 0 {
			classIndex = index
			break
		}
	}
	if classIndex < 0 {
		return errManagedCollectorScratch
	}
	pages := &heap.oldAvailable[classIndex]
	if len(*pages) == 0 {
		return errManagedCollectorScratch
	}
	pageID := (*pages)[len(*pages)-1]
	if pageID >= uint32(len(heap.pages)) || heap.pages[pageID] == nil {
		return errManagedCollectorLayout
	}
	page := heap.pages[pageID]
	if len(page.free) == 0 {
		*pages = (*pages)[:len(*pages)-1]
		return errManagedCollectorScratch
	}
	index := page.free[len(page.free)-1]
	page.free = page.free[:len(page.free)-1]
	if len(page.free) == 0 {
		*pages = (*pages)[:len(*pages)-1]
	}
	oldPage := heap.pages[entry.page]
	if oldPage == nil || uint64(entry.offset)+uint64(entry.size) > uint64(len(oldPage.data)) || uint64(index)*uint64(page.class)+uint64(entry.size) > uint64(len(page.data)) {
		return errManagedCollectorLayout
	}
	copy(page.data[index*page.class:], oldPage.data[entry.offset:entry.offset+entry.size])
	page.entries[index] = slot
	page.used += page.class
	entry.page, entry.offset, entry.young, entry.age = pageID, index*page.class, false, 0
	heap.youngBytes -= entry.logical
	collection.copiedBytes += uint64(entry.size)
	collection.promotedBytes += uint64(entry.size)
	return heap.markYoungReferences(entry)
}

// sweepOldParallel performs only the read-only liveness partition in workers.
// The queue backing array is already pre-admitted for every occupied slot, so
// workers do not allocate or mutate directory vectors. The serial removal
// phase preserves swap-delete invariants and old-page freelists.
func (heap *managedHeap) sweepOldParallel(epoch uint64, collection *managedCollection) error {
	if len(heap.occupied) == 0 {
		return nil
	}
	if cap(heap.queue) < len(heap.occupied) {
		return errManagedCollectorScratch
	}
	count := len(heap.occupied)
	heap.queue = heap.queue[:count]
	workers := 1
	if heap.collectorWorkers > 1 && count >= managedParallelSweepMinSlots {
		workers = min(heap.collectorWorkers, managedMaxCollectorWorkers, (count+managedParallelSweepChunk-1)/managedParallelSweepChunk)
	}
	chunk := (count + workers - 1) / workers
	if workers == 1 {
		for index := 0; index < count; index++ {
			slot := heap.occupied[index]
			entry := heap.object(slot)
			if entry != nil && entry.state.Load() == managedPublished && !entry.young && entry.marked != epoch {
				heap.queue[index] = slot
			} else {
				heap.queue[index] = 0
			}
		}
	} else {
		var group sync.WaitGroup
		group.Add(workers)
		for worker := 0; worker < workers; worker++ {
			start := worker * chunk
			end := min(count, start+chunk)
			go func(start, end int) {
				defer group.Done()
				for index := start; index < end; index++ {
					slot := heap.occupied[index]
					entry := heap.object(slot)
					if entry != nil && entry.state.Load() == managedPublished && !entry.young && entry.marked != epoch {
						heap.queue[index] = slot
					} else {
						heap.queue[index] = 0
					}
				}
			}(start, end)
		}
		group.Wait()
	}
	for index := 0; index < count; index++ {
		slot := heap.queue[index]
		if slot == 0 {
			continue
		}
		entry := heap.object(slot)
		if entry == nil || entry.state.Load() != managedPublished {
			return errManagedReference
		}
		collection.reclaimedBytes += entry.logical
		heap.freeObject(slot, true)
	}
	return nil
}

func (heap *managedHeap) reclaimEmptyPages() {
	for id, page := range heap.pages {
		if page == nil || page.used != 0 || page.kind != managedOldPage && page.kind != managedLargePage {
			continue
		}
		// Keep one empty page per size class as the pre-admitted promotion
		// cache.  Additional empty pages are reclaimed and become reusable;
		// retaining this bounded reserve keeps copying allocation-free.
		if page.kind == managedOldPage {
			classIndex := bits.TrailingZeros32(page.class) - 3
			if classIndex >= 0 && classIndex < len(heap.oldAvailable) && len(heap.oldAvailable[classIndex]) <= 1 {
				continue
			}
		}
		for index := range heap.oldAvailable {
			pages := heap.oldAvailable[index]
			write := 0
			for _, candidate := range pages {
				if candidate != uint32(id) {
					pages[write] = candidate
					write++
				}
			}
			heap.oldAvailable[index] = pages[:write]
		}
		if heap.counters.residentPageBytes >= page.resident {
			heap.counters.residentPageBytes -= page.resident
		} else {
			heap.counters.residentPageBytes = 0
		}
		page.data, page.entries, page.free, page.cards = nil, nil, nil, nil
		// The shared card-log capacity remains admitted when payload buffers
		// are released. Reusing this directory page must reuse that capacity.
		page.used, page.class, page.partner = 0, 0, 0
		page.kind, page.resident, page.leased = uint8(managedFree), 0, false
		heap.freePages = append(heap.freePages, uint32(id))
	}
}

func (heap *managedHeap) resetCards() {
	heap.pendingCards = heap.pendingCards[:0]
	for _, page := range heap.pages {
		if page == nil {
			continue
		}
		for index := range page.cards {
			page.cards[index].Store(0)
		}
	}
}

func (heap *managedHeap) markCard(pageID, offset, size uint32) error {
	if size == 0 {
		return nil
	}
	if pageID >= uint32(len(heap.pages)) || heap.pages[pageID] == nil {
		return errManagedCollectorLayout
	}
	page := heap.pages[pageID]
	if uint64(offset)+uint64(size) > uint64(len(page.data)) {
		return errManagedCollectorLayout
	}
	first, last := offset/managedCardBytes, (offset+size-1)/managedCardBytes
	if last >= uint32(len(page.cards)) {
		return errManagedCollectorLayout
	}
	for index := first; index <= last; index++ {
		if page.cards[index].CompareAndSwap(0, 1) {
			if len(heap.pendingCards) == cap(heap.pendingCards) {
				return errManagedCollectorScratch
			}
			heap.pendingCards = append(heap.pendingCards, managedCard{page: pageID, index: index})
		}
	}
	return nil
}

func (heap *managedHeap) markYoungReferences(entry *managedObject) error {
	if entry.layout == 0 || int(entry.layout) >= len(heap.layouts) {
		return errManagedCollectorLayout
	}
	layout := heap.layouts[entry.layout]
	page := heap.pages[entry.page]
	if page == nil || uint64(entry.offset)+uint64(entry.count)*uint64(layout.stride) > uint64(len(page.data)) {
		return errManagedCollectorLayout
	}
	for element := uint32(0); element < entry.count; element++ {
		base := entry.offset + element*layout.stride
		for _, offset := range layout.references {
			word := binary.LittleEndian.Uint64(page.data[base+offset:])
			child := guestRef{slot: uint32(word), generation: uint32(word >> 32)}
			if child.slot == 0 && child.generation == 0 {
				continue
			}
			resolved, err := heap.resolveObject(child)
			if err != nil {
				return err
			}
			if resolved.young {
				if err := heap.markCard(entry.page, base+offset, 8); err != nil {
					return err
				}
			}
		}
		for _, dynamic := range layout.dynamics {
			representation := binary.LittleEndian.Uint32(page.data[base+dynamic+4:])
			if representation != managedCollectorValueRef && representation != managedCollectorValueBox {
				continue
			}
			word := binary.LittleEndian.Uint64(page.data[base+dynamic+8:])
			child := guestRef{slot: uint32(word), generation: uint32(word >> 32)}
			if child.slot == 0 && child.generation == 0 {
				continue
			}
			resolved, err := heap.resolveObject(child)
			if err != nil {
				return err
			}
			if resolved.young {
				if err := heap.markCard(entry.page, base+dynamic+8, 8); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (heap *managedHeap) rebuildYoungCards() error {
	for _, slot := range heap.occupied {
		entry := heap.object(slot)
		if entry != nil && entry.state.Load() == managedPublished && !entry.young {
			if err := heap.markYoungReferences(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func (heap *managedHeap) refreshPendingCards() error {
	write := 0
	for _, card := range heap.pendingCards {
		page := heap.pages[card.page]
		start, end := card.index*managedCardBytes, (card.index+1)*managedCardBytes
		if end > uint32(len(page.data)) {
			end = uint32(len(page.data))
		}
		keep := false
		first, last := managedCardEntryRange(page, start, end)
		for index := first; index < last; index++ {
			slot := page.entries[index]
			entry := heap.object(slot)
			if entry == nil || entry.state.Load() != managedPublished || entry.page != card.page || !managedRangesOverlap(entry.offset, entry.size, start, end) {
				continue
			}
			if err := heap.cardHasYoung(entry, start, end, &keep); err != nil {
				return err
			}
		}
		if keep {
			heap.pendingCards[write] = card
			write++
		} else {
			page.cards[card.index].Store(0)
		}
	}
	heap.pendingCards = heap.pendingCards[:write]
	return nil
}

func (heap *managedHeap) cardHasYoung(entry *managedObject, start, end uint32, keep *bool) error {
	if entry.layout == 0 || int(entry.layout) >= len(heap.layouts) {
		return errManagedCollectorLayout
	}
	layout := heap.layouts[entry.layout]
	page := heap.pages[entry.page]
	if page == nil || uint64(entry.offset)+uint64(entry.count)*uint64(layout.stride) > uint64(len(page.data)) {
		return errManagedCollectorLayout
	}
	first, last := managedElementRange(entry, layout, start, end)
	for element := first; element < last; element++ {
		base := entry.offset + element*layout.stride
		referenceStart, _ := slices.BinarySearch(layout.references, uint32(max(0, int64(start)-int64(base)-7)))
		for _, offset := range layout.references[referenceStart:] {
			if base+offset >= end {
				break
			}
			if !managedRangesOverlap(base+offset, 8, start, end) {
				continue
			}
			word := binary.LittleEndian.Uint64(page.data[base+offset:])
			child := guestRef{slot: uint32(word), generation: uint32(word >> 32)}
			if child.slot == 0 && child.generation == 0 {
				continue
			}
			resolved, err := heap.resolveObject(child)
			if err != nil {
				return err
			}
			*keep = *keep || resolved.young
		}
		dynamicStart, _ := slices.BinarySearch(layout.dynamics, uint32(max(0, int64(start)-int64(base)-15)))
		for _, dynamic := range layout.dynamics[dynamicStart:] {
			if base+dynamic >= end {
				break
			}
			if !managedRangesOverlap(base+dynamic+4, 12, start, end) {
				continue
			}
			representation := binary.LittleEndian.Uint32(page.data[base+dynamic+4:])
			if representation != managedCollectorValueRef && representation != managedCollectorValueBox {
				continue
			}
			word := binary.LittleEndian.Uint64(page.data[base+dynamic+8:])
			child := guestRef{slot: uint32(word), generation: uint32(word >> 32)}
			if child.slot == 0 && child.generation == 0 {
				continue
			}
			resolved, err := heap.resolveObject(child)
			if err != nil {
				return err
			}
			*keep = *keep || resolved.young
		}
	}
	return nil
}
