package runtime

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"
)

func TestManagedControlReservationPublishesAndReleasesOnce(t *testing.T) {
	heap := newManagedHeap()
	heap.maxGuestBytes = 64
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	payload := managedTestObject(t, heap, layout, 1, 8)
	reservation, err := heap.reserveControl(56)
	if err != nil {
		t.Fatal(err)
	}
	before := heap.stats()
	if before.occupiedBytes != 8 || before.reservedBytes != 56 || before.totalAllocatedBytes != 8 {
		t.Fatalf("unpublished control storage = %+v", before)
	}
	if _, err := heap.reserveControl(1); !errors.Is(err, errManagedCollection) {
		t.Fatalf("reservation did not count toward quota: %v", err)
	}
	reservation.abort()
	reservation.abort()
	if after := heap.stats(); after.reservedBytes != 0 || after.totalAllocatedBytes != before.totalAllocatedBytes {
		t.Fatalf("abort changed committed history: %+v", after)
	}
	reservation, err = heap.reserveControl(56)
	if err != nil {
		t.Fatal(err)
	}
	reservation.commit()
	reservation.commit()
	reservation.abort()
	if after := heap.stats(); after.occupiedBytes != 64 || after.reservedBytes != 0 || after.allocations != 2 || after.totalAllocatedBytes != 64 {
		t.Fatalf("control commit was not once-only: %+v", after)
	}
	collection, err := heap.collect([]guestRef{payload}, true)
	if err != nil || collection.reachableBytes != 64 {
		t.Fatalf("major omitted control owner: %+v, %v", collection, err)
	}
	heap.releaseControl(56)
	if after := heap.stats(); after.occupiedBytes != 8 || after.totalAllocatedBytes != 64 || after.reclaimedBytes != 56 {
		t.Fatalf("control release consumed payload accounting: %+v", after)
	}
	heap.close()
	heap.releaseControl(56)
	if after := heap.stats(); after.occupiedBytes != 0 || after.reservedBytes != 0 || after.residentPageBytes != 0 {
		t.Fatalf("late owner cleanup changed closed accounting: %+v", after)
	}
}

func TestManagedControlAdmissionTransfersUnusedWorkerCredit(t *testing.T) {
	heap := newManagedHeap()
	heap.maxGuestBytes = 128
	layout := managedTestLayout(t, heap, managedLayout{stride: 16})
	batch, err := heap.acquireBatch(128, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := batch.reserve(layout, 1, 16)
	if err != nil {
		t.Fatal(err)
	}
	firstRef := first.commit()
	pending, err := batch.reserve(layout, 1, 16)
	if err != nil {
		t.Fatal(err)
	}
	control, err := heap.reserveControl(96, batch)
	if err != nil {
		t.Fatal(err)
	}
	if batch.credit != 32 || heap.stats().reservedBytes != 128 {
		t.Fatal("credit transfer changed total admission")
	}
	control.commit()
	pending.abort()
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	stats := heap.stats()
	if stats.occupiedBytes != 112 || stats.reservedBytes != 0 || stats.totalAllocatedBytes != 112 || stats.allocations != 2 {
		t.Fatalf("transferred ledger = %+v", stats)
	}
	if _, err := heap.collect([]guestRef{firstRef}, true); err != nil {
		t.Fatal(err)
	}
	heap.releaseControl(96)
	if stats := heap.stats(); stats.occupiedBytes != 16 || stats.totalAllocatedBytes != 112 {
		t.Fatalf("control owner release = %+v", stats)
	}
}

func TestManagedPayloadAdmissionTransfersUnusedWorkerCredit(t *testing.T) {
	heap := newManagedHeap()
	heap.maxGuestBytes = 128
	layout := managedTestLayout(t, heap, managedLayout{stride: 16})
	batch, err := heap.acquireBatch(128, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := batch.reserve(layout, 1, 16)
	if err != nil {
		t.Fatal(err)
	}
	firstRef := first.commit()
	// A large-page/direct reservation cannot fit the worker page, but its
	// unused lease credit is still part of this worker's admission budget.
	direct, err := heap.reserve(layout, 1, 96, batch)
	if err != nil {
		t.Fatalf("direct payload admission: %v", err)
	}
	directRef := direct.commit()
	if batch.credit != 32 || heap.stats().reservedBytes != 32 || heap.stats().occupiedBytes != 96 {
		t.Fatalf("payload credit transfer changed total admission: credit=%d stats=%+v", batch.credit, heap.stats())
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	stats := heap.stats()
	if stats.occupiedBytes != 112 || stats.reservedBytes != 0 || stats.totalAllocatedBytes != 112 {
		t.Fatalf("payload transfer ledger = %+v", stats)
	}
	if _, err := heap.resolveObject(firstRef); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.resolveObject(directRef); err != nil {
		t.Fatal(err)
	}
}

func TestFailedDirectAdmissionRestoresWorkerCredit(t *testing.T) {
	heap := newManagedHeap()
	heap.maxGuestBytes = 128
	layout := managedTestLayout(t, heap, managedLayout{stride: 16})
	batch, err := heap.acquireBatch(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := heap.reserve(layout, 1, 200, batch); !errors.Is(err, errManagedCollection) {
		t.Fatalf("oversized direct admission error = %v", err)
	}
	if batch.credit != 128 || heap.stats().reservedBytes != 128 {
		t.Fatalf("failed direct admission consumed lease credit: credit=%d stats=%+v", batch.credit, heap.stats())
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	if stats := heap.stats(); stats.occupiedBytes != 0 || stats.reservedBytes != 0 {
		t.Fatalf("failed direct admission leaked ledger: %+v", stats)
	}
}

func TestManagedAllocationBatchCreditsAbortAndFlush(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	batch, err := heap.acquireBatch(16, 2)
	if err != nil {
		t.Fatalf("acquire batch: %v", err)
	}
	first, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatalf("reserve first: %v", err)
	}
	firstRef := first.commit()
	second, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatalf("reserve second: %v", err)
	}
	second.abort()
	if err := batch.flush(); err != nil {
		t.Fatalf("flush batch: %v", err)
	}
	stats := heap.stats()
	if stats.occupiedBytes != 8 || stats.reservedBytes != 0 || stats.allocations != 1 {
		t.Fatalf("batch accounting = %+v", stats)
	}
	if _, err := heap.resolveObject(firstRef); err != nil {
		t.Fatalf("committed object unavailable: %v", err)
	}
	if _, err := heap.resolveObject(second.reference()); !errors.Is(err, errManagedReference) {
		t.Fatalf("aborted object resolved: %v", err)
	}
}

func TestManagedAllocationBatchRefillsAtLeaseBoundary(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	batch, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatal(err)
	}
	first, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	page := batch.page
	if err := batch.refill(managedBatchCredit, managedBatchHandles); err != nil {
		t.Fatalf("refill with pending reservation: %v", err)
	}
	first.commit()
	second, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatalf("reserve after lease boundary: %v", err)
	}
	second.commit()
	if batch.page == page || len(batch.pages) < 2 {
		t.Fatalf("lease was not refilled: page=%d old=%d pages=%v", batch.page, page, batch.pages)
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	if stats := heap.stats(); stats.allocations != 2 || stats.reservedBytes != 0 {
		t.Fatalf("refilled batch accounting: %+v", stats)
	}
}

func TestManagedAllocationRefillPreparesObservedPromotionClasses(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	batch, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	root := reservation.commit()
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect([]guestRef{root}, false); err != nil {
		t.Fatal(err)
	}
	refill, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := refill.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := heap.collect([]guestRef{root}, false); err != nil {
		t.Fatal(err)
	}
	entry, err := heap.resolveObject(root)
	if err != nil || entry.young {
		t.Fatalf("batch-only survivor did not promote: entry=%+v, err=%v", entry, err)
	}
}

func TestManagedAllocationConcurrentBatchesPublishLinkedPayloads(t *testing.T) {
	const workers, objects = 8, 128
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	parent := managedTestObject(t, heap, layout, workers, workers*8)
	for range 2 {
		if _, err := heap.collect([]guestRef{parent}, false); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			batch, err := heap.acquireBatch(objects*8, objects)
			if err != nil {
				t.Error(err)
				return
			}
			var previous guestRef
			for range objects {
				reservation, err := batch.reserve(layout, 1, 8)
				if err != nil {
					t.Error(err)
					return
				}
				binary.LittleEndian.PutUint64(reservation.bytes(), uint64(previous.slot)|uint64(previous.generation)<<32)
				previous = reservation.commit()
			}
			var ticket [8]byte
			binary.LittleEndian.PutUint64(ticket[:], uint64(previous.slot)|uint64(previous.generation)<<32)
			if err := batch.write(parent, uint32(worker*8), ticket[:]); err != nil {
				t.Error(err)
			}
			if err := batch.flush(); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if t.Failed() {
		return
	}
	if _, err := heap.collect([]guestRef{parent}, false); err != nil {
		t.Fatal(err)
	}
	for worker := range workers {
		ref, err := heap.readReference(parent, uint32(worker*8))
		for index := 0; index < objects; index++ {
			if err != nil || !ref.valid() {
				t.Fatalf("worker %d chain stopped at %d: %v", worker, index, err)
			}
			ref, err = heap.readReference(ref, 0)
		}
		if err != nil || ref.valid() {
			t.Fatalf("worker %d chain tail: %+v, %v", worker, ref, err)
		}
	}
	if stats := heap.stats(); stats.allocations != 1+workers*objects || stats.reservedBytes != 0 {
		t.Fatalf("concurrent publication accounting: %+v", stats)
	}
}

func TestManagedAllocationBatchLogsOldCardBeforeFlush(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8, references: []uint32{0}})
	old := managedTestObject(t, heap, layout, 1, 8)
	if _, err := heap.collect([]guestRef{old}, true); err != nil {
		t.Fatalf("first major: %v", err)
	}
	if _, err := heap.collect([]guestRef{old}, true); err != nil {
		t.Fatalf("promotion major: %v", err)
	}
	batch, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatalf("acquire batch: %v", err)
	}
	if err := batch.write(old, 0, make([]byte, 8)); err != nil {
		t.Fatalf("batch old write: %v", err)
	}
	if len(heap.pendingCards) != 0 {
		t.Fatal("batch write published card before flush")
	}
	if err := batch.flush(); err != nil {
		t.Fatalf("flush card log: %v", err)
	}
	if len(heap.pendingCards) == 0 {
		t.Fatal("batch card log was lost at flush")
	}
}

func TestManagedAllocationBatchesLeaseSeparateNurseryPages(t *testing.T) {
	heap := newManagedHeap()
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	first, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	second, err := heap.acquireBatch(8, 1)
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if first.page == second.page {
		t.Fatalf("leased nursery page reused: first=%d second=%d", first.page, second.page)
	}
	for _, batch := range []*managedAllocationBatch{first, second} {
		reservation, err := batch.reserve(layout, 1, 8)
		if err != nil {
			t.Fatalf("reserve batch page %d: %v", batch.page, err)
		}
		reservation.commit()
		if err := batch.flush(); err != nil {
			t.Fatalf("flush batch page %d: %v", batch.page, err)
		}
	}
	if heap.activeBatches != 0 {
		t.Fatalf("active batches after flush: %d", heap.activeBatches)
	}
}

func TestManagedAllocationBatchIsConsumedByManagedValuePublication(t *testing.T) {
	module := newManagedValueTestModule()
	batch, err := module.vm.heap.acquireBatch(64, 8)
	if err != nil {
		t.Fatal(err)
	}
	value := module.adoptValueWithBatch(newVMValue("String", "worker lease"), batch)
	if !value.ref.valid() || value.heap != module.vm.heap {
		t.Fatalf("managed value was not published through the worker lease: %#v", value)
	}
	if batch.committed == 0 {
		t.Fatal("managed value publication did not consume the worker lease")
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	if module.vm.heap.activeBatches != 0 {
		t.Fatalf("active batches after flush = %d", module.vm.heap.activeBatches)
	}
}
