package runtime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"unsafe"
)

func TestManagedDirectoryGrowthPreservesPublishedViewsAndResidentAdmission(t *testing.T) {
	heap := newManagedHeap()
	t.Cleanup(heap.close)
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	ref := managedTestObject(t, heap, layout, 1, 8)
	if err := heap.write(ref, 0, []byte("retained")); err != nil {
		t.Fatal(err)
	}
	published := heap.directory.Load()
	original := published.segments[0]
	for range 12 {
		before := heap.stats().residentPageBytes
		pointerCapacity := cap(heap.segments)
		vectorCapacity := cap(heap.freeSlots) + cap(heap.occupied) + cap(heap.youngSlots) + cap(heap.queue)
		if err := heap.growDirectory(); err != nil {
			t.Fatal(err)
		}
		growth := uint64(unsafe.Sizeof([managedSegmentSize]managedObject{}))
		growth += uint64(cap(heap.segments)-pointerCapacity) * uint64(unsafe.Sizeof(uintptr(0)))
		growth += uint64(cap(heap.freeSlots)+cap(heap.occupied)+cap(heap.youngSlots)+cap(heap.queue)-vectorCapacity) * 4
		if got := heap.stats().residentPageBytes; got != before+growth {
			t.Fatalf("directory resident charge = %d, want %d", got, before+growth)
		}
		if len(published.segments) != 1 || published.segments[0] != original {
			t.Fatal("growth changed an already published directory view")
		}
	}
	before := heap.stats()
	directory := heap.directory.Load()
	heap.maxResidentBytes = before.residentPageBytes
	if err := heap.growDirectory(); !errors.Is(err, errManagedLimit) {
		t.Fatalf("growth past resident budget: %v", err)
	}
	if heap.stats() != before || heap.directory.Load() != directory {
		t.Fatal("failed growth changed storage or accounting")
	}
	heap.maxResidentBytes = 0
	if err := heap.growDirectory(); err != nil {
		t.Fatalf("growth retry: %v", err)
	}
	var data [8]byte
	if err := heap.read(ref, 0, data[:]); err != nil || string(data[:]) != "retained" {
		t.Fatalf("original object after growth = %q, %v", data, err)
	}
}

func TestManagedHeapStringReadOwnsBytesAcrossMutationAndCollection(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	ref := managedTestObject(t, heap, layout, 1, 8)
	if err := heap.write(ref, 0, []byte{'a', 0xff, 0, 'z'}); err != nil {
		t.Fatal(err)
	}
	text, err := heap.readString(ref, 1, 3)
	if err != nil || text != "\xff\x00z" {
		t.Fatalf("string = %q, %v", text, err)
	}
	if err := heap.clear(ref, 0, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect([]guestRef{ref}, false); err != nil {
		t.Fatal(err)
	}
	if text != "\xff\x00z" {
		t.Fatalf("detached string changed: %q", text)
	}
	for _, span := range [][2]uint64{{7, 2}, {9, 0}, {1, ^uint64(0)}} {
		if _, err := heap.readString(ref, span[0], span[1]); err == nil {
			t.Fatalf("out-of-range span %v accepted", span)
		}
	}
}

func TestManagedHeapCopyPreservesOverlapAndRemembersReferences(t *testing.T) {
	heap := newManagedHeap()
	bytesLayout := managedTestLayout(t, heap, managedLayout{stride: 8})
	buffer := managedTestObject(t, heap, bytesLayout, 2, 16)
	if err := heap.write(buffer, 0, []byte("abcdefghijklmnop")); err != nil {
		t.Fatal(err)
	}
	if err := heap.copy(buffer, 4, buffer, 0, 12); err != nil {
		t.Fatal(err)
	}
	var result [16]byte
	if err := heap.read(buffer, 0, result[:]); err != nil || !bytes.Equal(result[:], []byte("abcdabcdefghijkl")) {
		t.Fatalf("overlapping copy = %q, %v", result, err)
	}
	refs := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	old := managedTestObject(t, heap, refs, 1, 8)
	for range 2 {
		if _, err := heap.collect([]guestRef{old}, false); err != nil {
			t.Fatal(err)
		}
	}
	child := managedTestObject(t, heap, refs, 1, 8)
	source := managedTestObject(t, heap, refs, 1, 8)
	managedTestReference(t, heap, source, 0, child)
	if err := heap.copy(old, 0, source, 0, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect([]guestRef{old}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.resolveObject(child); err != nil {
		t.Fatalf("bulk copy did not remember child: %v", err)
	}
	if err := heap.clear(old, 0, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect([]guestRef{old}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.resolveObject(child); !errors.Is(err, errManagedReference) {
		t.Fatalf("clear retained child: %v", err)
	}
}

func TestManagedHeapSwapPreservesReferenceAcrossCards(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	parent := managedTestObject(t, heap, layout, 64, 512)
	for range 2 {
		if _, err := heap.collect([]guestRef{parent}, true); err != nil {
			t.Fatal(err)
		}
	}
	child := managedTestObject(t, heap, layout, 1, 8)
	managedTestReference(t, heap, parent, 0, child)
	if err := heap.swap(parent, 0, 264, 8); err != nil {
		t.Fatal(err)
	}
	if got, err := heap.readReference(parent, 0); err != nil || got.valid() {
		t.Fatalf("first swapped field = %v, %v", got, err)
	}
	if got, err := heap.readReference(parent, 264); err != nil || got != child {
		t.Fatalf("second swapped field = %v, %v", got, err)
	}
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.resolveObject(child); err != nil {
		t.Fatalf("swapped reference was lost across cards: %v", err)
	}
}

func managedTestLayout(t *testing.T, heap *managedHeap, layout managedLayout) uint32 {
	t.Helper()
	id, err := heap.registerLayout(layout)
	if err != nil {
		t.Fatalf("register layout: %v", err)
	}
	return id
}

func managedTestObject(t *testing.T, heap *managedHeap, layout, count uint32, logical uint64) guestRef {
	t.Helper()
	reservation, err := heap.reserve(layout, count, logical)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return reservation.commit()
}

func managedTestReference(t *testing.T, heap *managedHeap, parent guestRef, offset uint32, child guestRef) {
	t.Helper()
	if err := heap.writeReference(parent, offset, child); err != nil {
		t.Fatalf("write reference: %v", err)
	}
}

func TestManagedLayoutOwnershipAcrossConstructionAndCollection(t *testing.T) {
	heap := newManagedHeap()
	shape := managedLayout{stride: 16, references: []uint32{0, 8}}
	id := managedTestLayout(t, heap, shape)
	if _, err := heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	// Construction owns the descriptor even before an object is published.
	value := managedTestObject(t, heap, id, 1, 16)
	heap.releaseLayout(id)
	if _, err := heap.collect([]guestRef{value}, true); err != nil {
		t.Fatal(err)
	}
	if heap.layouts[id].stride != shape.stride {
		t.Fatal("published object lost its physical layout")
	}
	if err := heap.release(value); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect(nil, false); err != nil {
		t.Fatal(err)
	}
	if heap.layouts[id].stride != shape.stride {
		t.Fatal("minor collection swept the layout directory")
	}
	if _, err := heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	if heap.layouts[id].stride != 0 || heap.layouts[id].references != nil {
		t.Fatal("unowned descriptor retained its reference offsets")
	}
	reused := managedTestLayout(t, heap, managedLayout{stride: 8})
	if reused != id {
		t.Fatal("reclaimed layout directory entry was not reused")
	}
	managedTestObject(t, heap, reused, 1, 8)
}

func TestManagedLayoutPinsReleaseAfterAbortedPublication(t *testing.T) {
	heap := newManagedHeap()
	id := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	other := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	if other != id {
		t.Fatal("identical physical layouts were not shared")
	}
	heap.releaseLayout(id)
	if _, err := heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	if heap.layouts[id].stride == 0 {
		t.Fatal("one constructor released another constructor's pin")
	}
	heap.releaseLayout(other)
	before := heap.stats().residentPageBytes
	if _, err := heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	if heap.layouts[id].stride != 0 || heap.stats().residentPageBytes+4 != before {
		t.Fatal("aborted construction did not release its descriptor and offset allocation")
	}
}

func TestManagedHeapReclaimHook(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	value := managedTestObject(t, heap, layout, 1, 8)
	var reclaimed guestRef
	heap.onReclaim = func(ref guestRef) { reclaimed = ref }
	if err := heap.release(value); err != nil {
		t.Fatalf("release value: %v", err)
	}
	if reclaimed != value {
		t.Fatalf("reclaim hook = %+v, want %+v", reclaimed, value)
	}
}

func TestManagedHeapCollectorRejectsStaleRootAndReusesGeneration(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	first := managedTestObject(t, heap, layout, 1, 8)
	if err := heap.release(first); err != nil {
		t.Fatalf("release: %v", err)
	}
	second := managedTestObject(t, heap, layout, 1, 8)
	if first.slot != second.slot || first.generation == second.generation {
		t.Fatalf("slot generation did not advance: first=%+v second=%+v", first, second)
	}
	if _, err := heap.collect([]guestRef{first}, true); !errors.Is(err, errManagedReference) {
		t.Fatalf("stale root error = %v, want %v", err, errManagedReference)
	}
	if _, err := heap.collect([]guestRef{second}, true); err != nil {
		t.Fatalf("current root collection: %v", err)
	}
}

func TestManagedHeapMajorWorkerCountsPreserveReachableGraph(t *testing.T) {
	for _, workers := range []int{1, 4} {
		heap := newManagedHeap()
		heap.collectorWorkers = workers
		layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
		roots := make([]guestRef, 300)
		for index := range roots {
			roots[index] = managedTestObject(t, heap, layout, 1, 8)
		}
		for index, root := range roots {
			managedTestReference(t, heap, root, 0, roots[(index+1)%len(roots)])
		}
		managedTestObject(t, heap, layout, 1, 8)
		collection, err := heap.collect(roots, true)
		if err != nil {
			t.Fatalf("%d workers: %v", workers, err)
		}
		if collection.reachableBytes != uint64(len(roots))*8 || collection.reclaimedBytes != 8 || heap.stats().occupiedBytes != uint64(len(roots))*8 {
			t.Fatalf("%d workers: collection=%+v stats=%+v", workers, collection, heap.stats())
		}
		for index, root := range roots {
			child, err := heap.readReference(root, 0)
			if err != nil || child != roots[(index+1)%len(roots)] {
				t.Fatalf("%d workers changed edge %d: %v, %v", workers, index, child, err)
			}
		}
		heap.close()
	}
}

func TestManagedHeapMinorCopiesCyclesAndPoisonsFromSpace(t *testing.T) {
	heap := newManagedHeap()
	heap.poisonCollected = true
	layout := managedTestLayout(t, heap, managedLayout{stride: 16, references: []uint32{0, 8}})
	first := managedTestObject(t, heap, layout, 1, 16)
	second := managedTestObject(t, heap, layout, 1, 16)
	managedTestReference(t, heap, first, 0, second)
	managedTestReference(t, heap, second, 0, first)
	fromPage := heap.object(first.slot).page
	collection, err := heap.collect([]guestRef{first}, false)
	if err != nil {
		t.Fatalf("minor collection: %v", err)
	}
	if collection.copiedBytes != 32 || collection.reclaimedBytes != 0 {
		t.Fatalf("cycle collection = %+v", collection)
	}
	if heap.object(first.slot).page == fromPage || heap.object(second.slot).page == fromPage {
		t.Fatal("survivor remained in from-space")
	}
	if heap.pages[fromPage].kind != managedSparePage || heap.pages[fromPage].data[0] != managedCopyPoison {
		t.Fatal("from-space was not poisoned after copying")
	}
	if got, err := heap.readReference(first, 0); err != nil || got != second {
		t.Fatalf("cycle reference changed: got=%+v err=%v want=%+v", got, err, second)
	}
}

func TestManagedHeapMinorReclaimsUnreachableYoung(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	live := managedTestObject(t, heap, layout, 1, 8)
	dead := managedTestObject(t, heap, layout, 1, 8)
	collection, err := heap.collect([]guestRef{live}, false)
	if err != nil {
		t.Fatalf("minor collection: %v", err)
	}
	if _, err := heap.resolveObject(dead); !errors.Is(err, errManagedReference) {
		t.Fatalf("dead object resolved: %v", err)
	}
	if collection.reclaimedBytes != 8 || heap.stats().occupiedBytes != 8 {
		t.Fatalf("reclamation = %+v stats=%+v", collection, heap.stats())
	}
}

func TestManagedHeapPromotionAndOldCardKeepsYoungAlive(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	parent := managedTestObject(t, heap, layout, 1, 8)
	child := managedTestObject(t, heap, layout, 1, 8)
	managedTestReference(t, heap, parent, 0, child)
	if _, err := heap.collect([]guestRef{parent}, true); err != nil {
		t.Fatalf("first major: %v", err)
	}
	if _, err := heap.collect([]guestRef{parent}, true); err != nil {
		t.Fatalf("promotion major: %v", err)
	}
	if heap.object(parent.slot).young || heap.object(child.slot).young {
		t.Fatalf("objects were not promoted: parent=%+v child=%+v", *heap.object(parent.slot), *heap.object(child.slot))
	}
	newChild := managedTestObject(t, heap, layout, 1, 8)
	managedTestReference(t, heap, parent, 0, newChild)
	if len(heap.pendingCards) == 0 {
		t.Fatal("old-to-young write did not enqueue a card")
	}
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatalf("minor after promotion: %v", err)
	}
	if _, err := heap.resolveObject(newChild); err != nil {
		t.Fatalf("young child lost through old card: %v", err)
	}
	if !heap.object(newChild.slot).young {
		t.Fatal("minor unexpectedly promoted young child")
	}
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatalf("second minor after promotion: %v", err)
	}
	if _, err := heap.resolveObject(newChild); err != nil {
		t.Fatalf("old card did not preserve young child across minors: %v", err)
	}
	if err := heap.writeWord(parent, 0, 0); err != nil {
		t.Fatalf("clear promoted edge: %v", err)
	}
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatalf("minor after clearing card: %v", err)
	}
	if len(heap.pendingCards) != 0 {
		t.Fatalf("card remained dirty without young edge: %d", len(heap.pendingCards))
	}
}

func TestManagedHeapOldToYoungToYoungCardDrainsTransitively(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	parent := managedTestObject(t, heap, layout, 1, 8)
	if _, err := heap.collect([]guestRef{parent}, true); err != nil {
		t.Fatalf("initial major: %v", err)
	}
	if _, err := heap.collect([]guestRef{parent}, true); err != nil {
		t.Fatalf("promote parent: %v", err)
	}
	if heap.object(parent.slot).young {
		t.Fatal("parent did not promote")
	}
	child := managedTestObject(t, heap, layout, 1, 8)
	grandchild := managedTestObject(t, heap, layout, 1, 8)
	managedTestReference(t, heap, parent, 0, child)
	managedTestReference(t, heap, child, 0, grandchild)
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatalf("transitive minor: %v", err)
	}
	if _, err := heap.resolveObject(child); err != nil {
		t.Fatalf("child lost through old card: %v", err)
	}
	if _, err := heap.resolveObject(grandchild); err != nil {
		t.Fatalf("grandchild lost after card queue drain: %v", err)
	}
}

func TestManagedHeapDuplicateRootsDoNotConsumeScratch(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	root := managedTestObject(t, heap, layout, 1, 8)
	heap.queue = make([]uint32, 0, len(heap.occupied))
	roots := make([]guestRef, 128)
	for index := range roots {
		roots[index] = root
	}
	if _, err := heap.collect(roots, false); err != nil {
		t.Fatalf("duplicate roots collection: %v", err)
	}
}

func TestManagedHeapRandomGraphMatchesReachabilityOracle(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 16, references: []uint32{0, 8}})
	const objectCount = 192
	objects := make([]guestRef, objectCount)
	for index := range objects {
		objects[index] = managedTestObject(t, heap, layout, 1, 16)
	}
	seed := uint32(0x9e3779b9)
	next := func() uint32 {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		return seed
	}
	graph := make([][]int, objectCount)
	for index := range objects {
		for edge := 0; edge < 2; edge++ {
			target := int(next() % objectCount)
			graph[index] = append(graph[index], target)
			managedTestReference(t, heap, objects[index], uint32(edge*8), objects[target])
		}
	}
	roots := []int{0, 17, 17, 91, 150, 191}
	reachable := make([]bool, objectCount)
	var visit func(int)
	visit = func(index int) {
		if reachable[index] {
			return
		}
		reachable[index] = true
		for _, target := range graph[index] {
			visit(target)
		}
	}
	rootRefs := make([]guestRef, len(roots))
	for index, root := range roots {
		visit(root)
		rootRefs[index] = objects[root]
	}
	if _, err := heap.collect(rootRefs, false); err != nil {
		t.Fatalf("random graph collection: %v", err)
	}
	for index, ref := range objects {
		_, err := heap.resolveObject(ref)
		if reachable[index] && err != nil {
			t.Fatalf("reachable object %d was reclaimed: %v", index, err)
		}
		if !reachable[index] && !errors.Is(err, errManagedReference) {
			t.Fatalf("unreachable object %d survived: %v", index, err)
		}
	}
}

func TestManagedHeapMultiPageNurseryCollection(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 16})
	const objectCount = 9000
	for index := 0; index < objectCount; index++ {
		managedTestObject(t, heap, layout, 1, 16)
	}
	if len(heap.nurseryPages) < 2 {
		t.Fatalf("nursery did not span pages: %d", len(heap.nurseryPages))
	}
	collection, err := heap.collect(nil, false)
	if err != nil {
		t.Fatalf("multi-page collection: %v", err)
	}
	if collection.reclaimedBytes != objectCount*16 || heap.stats().occupiedBytes != 0 {
		t.Fatalf("multi-page reclamation = %+v stats=%+v", collection, heap.stats())
	}
}

func TestManagedHeapMajorReclaimsEmptyLargePage(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: managedPageBytes / 2})
	object := managedTestObject(t, heap, layout, 1, uint64(managedPageBytes/2))
	pageID := heap.object(object.slot).page
	if heap.pages[pageID].kind != managedLargePage {
		t.Fatalf("large object used page kind %d", heap.pages[pageID].kind)
	}
	if _, err := heap.collect(nil, true); err != nil {
		t.Fatalf("major large sweep: %v", err)
	}
	if _, err := heap.resolveObject(object); !errors.Is(err, errManagedReference) {
		t.Fatalf("large object survived sweep: %v", err)
	}
	page := heap.pages[pageID]
	if page.kind != uint8(managedFree) || page.data != nil || len(page.entries) != 0 || len(heap.freePages) == 0 {
		t.Fatalf("empty large page was not released: kind=%d data=%d entries=%d free=%d", page.kind, len(page.data), len(page.entries), len(heap.freePages))
	}
}

func TestManagedHeapMajorSweepsOldAndYoungGraph(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	root := managedTestObject(t, heap, layout, 1, 8)
	oldGarbage := managedTestObject(t, heap, layout, 1, 8)
	managedTestReference(t, heap, root, 0, oldGarbage)
	if _, err := heap.collect([]guestRef{root}, true); err != nil {
		t.Fatalf("first major: %v", err)
	}
	if _, err := heap.collect([]guestRef{root}, true); err != nil {
		t.Fatalf("second major: %v", err)
	}
	if err := heap.writeWord(root, 0, 0); err != nil {
		t.Fatalf("clear old edge: %v", err)
	}
	collection, err := heap.collect([]guestRef{root}, true)
	if err != nil {
		t.Fatalf("sweep major: %v", err)
	}
	if _, err := heap.resolveObject(oldGarbage); !errors.Is(err, errManagedReference) {
		t.Fatalf("unreachable old object survived: %v", err)
	}
	if collection.reclaimedBytes != 8 {
		t.Fatalf("major reclaimed=%d want 8", collection.reclaimedBytes)
	}
}

func TestManagedHeapDynamicReferenceTagsControlTracing(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 32, dynamics: []uint32{0}})
	parent := managedTestObject(t, heap, layout, 1, 32)
	child := managedTestObject(t, heap, layout, 1, 32)
	parentEntry := heap.object(parent.slot)
	bytes := heap.pages[parentEntry.page].data[parentEntry.offset:]
	binary.LittleEndian.PutUint32(bytes[4:], managedCollectorValueRef)
	binary.LittleEndian.PutUint64(bytes[8:], uint64(child.slot)|uint64(child.generation)<<32)
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatalf("dynamic ref minor: %v", err)
	}
	if _, err := heap.resolveObject(child); err != nil {
		t.Fatalf("dynamic ref was not traced: %v", err)
	}
	bitsParent := managedTestObject(t, heap, layout, 1, 32)
	bitsChild := managedTestObject(t, heap, layout, 1, 32)
	bitsEntry := heap.object(bitsParent.slot)
	bits := heap.pages[bitsEntry.page].data[bitsEntry.offset:]
	binary.LittleEndian.PutUint32(bits[4:], 1)
	binary.LittleEndian.PutUint64(bits[8:], uint64(bitsChild.slot)|uint64(bitsChild.generation)<<32)
	if _, err := heap.collect([]guestRef{bitsParent}, false); err != nil {
		t.Fatalf("dynamic bits minor: %v", err)
	}
	if _, err := heap.resolveObject(bitsChild); !errors.Is(err, errManagedReference) {
		t.Fatalf("bits payload was treated as a reference: %v", err)
	}
}

func TestManagedHeapDynamicTagWriteAcrossCardBoundary(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 512, dynamics: []uint32{248}})
	parent := managedTestObject(t, heap, layout, 1, 512)
	for range 2 {
		if _, err := heap.collect([]guestRef{parent}, false); err != nil {
			t.Fatal(err)
		}
	}
	child := managedTestObject(t, heap, layout, 1, 512)
	managedTestReference(t, heap, parent, 256, child)
	// The dormant ticket is scalar bits until the tag is published. First
	// retire the card written by the ticket while keeping its child rooted.
	if _, err := heap.collect([]guestRef{parent, child}, false); err != nil {
		t.Fatal(err)
	}
	var tag [4]byte
	binary.LittleEndian.PutUint32(tag[:], managedCollectorValueRef)
	if err := heap.write(parent, 252, tag[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.resolveObject(child); err != nil {
		t.Fatalf("tag-only write lost child in adjacent card: %v", err)
	}
}

func TestManagedHeapCollectorScratchAndAbortArePreAdmitted(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	reservation, err := heap.reserve(layout, 1, 8)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	ref := reservation.reference()
	if _, err := heap.collect(nil, false); !errors.Is(err, errManagedCollectorBusy) {
		t.Fatalf("unpublished collection error=%v", err)
	}
	reservation.abort()
	if heap.stats().reservedBytes != 0 || heap.stats().occupiedBytes != 0 {
		t.Fatalf("abort left accounting: %+v", heap.stats())
	}
	managedTestObject(t, heap, layout, 1, 8)
	heap.queue = nil
	if _, err := heap.collect(nil, false); !errors.Is(err, errManagedCollectorScratch) {
		t.Fatalf("scratch exhaustion error=%v", err)
	}
	if _, err := heap.resolveObject(ref); !errors.Is(err, errManagedReference) {
		t.Fatalf("aborted reference resolved: %v", err)
	}
}

func TestManagedHeapCollectorRejectsActiveAllocationBatch(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	batch, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatalf("acquire batch: %v", err)
	}
	reservation, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatalf("batch reserve: %v", err)
	}
	ref := reservation.reference()
	reservation.commit()
	if _, err := heap.collect(nil, false); !errors.Is(err, errManagedCollectorBusy) {
		t.Fatalf("active batch collection error=%v", err)
	}
	if err := batch.flush(); err != nil {
		t.Fatalf("flush batch: %v", err)
	}
	if _, err := heap.collect([]guestRef{ref}, false); err != nil {
		t.Fatalf("collection after batch flush: %v", err)
	}
}
