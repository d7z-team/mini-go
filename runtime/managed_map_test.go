package runtime

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestManagedRollbackReleasesOwnerAndWorkerAllocations(t *testing.T) {
	heap := newManagedHeap()
	t.Cleanup(heap.close)
	layout := managedTestLayout(t, heap, managedLayout{stride: 8})
	batch, err := heap.acquireBatch(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	workerRef := reservation.commit()
	// A composite may outgrow the worker lease and publish another piece
	// through the owner allocator before a later construction step fails.
	ownerRef := managedTestObject(t, heap, layout, 1, 8)
	rollbackManagedPublications(heap, batch, ownerRef, workerRef)
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	stats := heap.stats()
	if stats.occupiedBytes != 0 || stats.reservedBytes != 0 || heap.youngBytes != 0 {
		t.Fatalf("failed construction retained allocation accounting: %+v, young=%d", stats, heap.youngBytes)
	}
	if stats.totalAllocatedBytes != 16 || stats.allocations != 2 || stats.reclaimedBytes != 16 {
		t.Fatalf("rollback lost successful allocation history: %+v", stats)
	}
	for _, ref := range []guestRef{workerRef, ownerRef} {
		if _, err := heap.resolveObject(ref); err == nil {
			t.Fatal("failed construction left a published payload")
		}
	}
	if _, err := heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	// Once a batch has returned to the owner, a retained construction context
	// must use owner accounting when releasing its published records.
	batch, err = heap.acquireBatch(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err = batch.reserve(layout, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	workerRef = reservation.commit()
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	rollbackManagedPublications(heap, batch, workerRef)
	stats = heap.stats()
	if stats.occupiedBytes != 0 || heap.youngBytes != 0 || stats.totalAllocatedBytes != 24 || stats.allocations != 3 || stats.reclaimedBytes != 24 {
		t.Fatalf("returned batch rollback accounting: %+v, young=%d", stats, heap.youngBytes)
	}
}

func TestManagedMapIndexRehashDeleteAndReinsertPreserveIdentityRules(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, Int>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key := int64(0); key < 40; key++ {
		if _, err := setIndexValue(module, value, newVMValue("Int", key), newVMValue("Int", key+100)); err != nil {
			t.Fatalf("insert %d: %v", key, err)
		}
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map did not materialize as managed storage")
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 17})
	if !found || entry.Value.materializedData() != int64(117) {
		t.Fatalf("indexed lookup = %#v, found=%v", entry, found)
	}
	oldIdentity := entry.identity
	if err := deleteValue(module, value, newVMValue("Int", int64(17))); err != nil {
		t.Fatal(err)
	}
	if _, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 17}); found {
		t.Fatal("deleted map key remained in bucket index")
	}
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(17)), newVMValue("Int", int64(217))); err != nil {
		t.Fatal(err)
	}
	entry, found = managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 17})
	if !found || entry.Value.materializedData() != int64(217) || entry.identity == oldIdentity {
		t.Fatalf("reinserted entry = %#v, old identity=%d", entry, oldIdentity)
	}
}

func TestManagedMapScalarUpdateAtGuestLimit(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, Int>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := newVMValue("Int", int64(7))
	if _, err := setIndexValue(module, value, key, newVMValue("Int", int64(1))); err != nil {
		t.Fatal(err)
	}
	before := module.vm.heap.stats()
	module.vm.heap.maxGuestBytes = before.occupiedBytes
	if _, err := setIndexValue(module, value, key, newVMValue("Int", int64(42))); err != nil {
		t.Fatalf("in-place scalar update: %v", err)
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map storage missing")
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 7})
	if !found || entry.Value.materializedData() != int64(42) {
		t.Fatalf("updated entry = %#v, found=%v", entry, found)
	}
	after := module.vm.heap.stats()
	if after.occupiedBytes != before.occupiedBytes || after.reservedBytes != 0 || after.allocations != before.allocations {
		t.Fatalf("scalar update changed storage accounting: before=%+v after=%+v", before, after)
	}
}

func TestManagedMapIdentityCarriesFullPreparedTypeID(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, String>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := module.vm.valueLayouts.layoutFor(value.Type)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := module.vm.heap.resolveObject(value.ref)
	if err != nil {
		t.Fatal(err)
	}
	if got := module.vm.heap.layouts[entry.layout].typeID; got != want.ID {
		t.Fatalf("managed map type id = %d, want %d", got, want.ID)
	}
}

func TestManagedMapStorageOwnsKeyAndValueTypeClosures(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Array<2, Int>, Array<2, Int>>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	managed, ok := materializeManagedMap(value)
	if !ok || managed == nil || managed.managed == nil {
		t.Fatal("map did not materialize")
	}
	if managed.managed.keyType.Table == value.Type.Table || managed.managed.valueType.Table == value.Type.Table {
		t.Fatal("managed map retained the source type table")
	}
}

func TestManagedMapMetadataRetainsNestedPreparedLayouts(t *testing.T) {
	module := newManagedValueTestModule()
	typ := "Int"
	var outer vmValue
	var ids []uint32
	for range 12 {
		typ = "Map<Int," + typ + ">"
		var err error
		outer, err = newMapValueWithCapacityBatch(module, typ, nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		layout, err := module.vm.valueLayouts.layoutFor(outer.Type)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, layout.ID)
	}
	if _, err := module.vm.heap.collect([]guestRef{outer.ref}, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, ok := module.vm.valueLayouts.byID(id); !ok {
			t.Fatalf("live map descriptor lost nested layout %d", id)
		}
	}
	if _, err := module.vm.heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	if len(module.vm.valueLayouts.mapLayouts) != 0 {
		t.Fatal("unowned map metadata remained after major collection")
	}
}

func TestManagedMapRollsBackIdentityWhenBackingAdmissionFails(t *testing.T) {
	module := newManagedValueTestModule()
	// The identity record fits, but its entry and index records do not. A
	// failed composite build must release the already-published identity too.
	module.vm.heap.maxGuestBytes = 16
	if _, err := newMapValueWithCapacityBatch(module, "Map<Int, Int>", nil, 1, nil); err == nil {
		t.Fatal("map construction unexpectedly succeeded past the guest budget")
	}
	stats := module.vm.heap.stats()
	if stats.occupiedBytes != 0 || stats.reservedBytes != 0 {
		t.Fatalf("failed map construction retained admission: occupied=%d reserved=%d", stats.occupiedBytes, stats.reservedBytes)
	}
}

func TestManagedMapGrowthFailurePreservesExistingEntries(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, Int>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key := int64(0); key < 64; key++ {
		if _, err := setIndexValue(module, value, newVMValue("Int", key), newVMValue("Int", key+100)); err != nil {
			t.Fatalf("insert %d: %v", key, err)
		}
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map did not materialize")
	}
	before := module.vm.heap.stats()
	module.vm.heap.maxGuestBytes = before.occupiedBytes
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(64)), newVMValue("Int", int64(164))); err == nil {
		t.Fatal("65th entry unexpectedly grew a map at the hard budget")
	}
	if got := managed.managed.length(); got != 64 {
		t.Fatalf("failed growth changed map length: %d", got)
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 63})
	if !found || entry.Value.materializedData() != int64(163) {
		t.Fatalf("failed growth damaged existing entry: %#v found=%t", entry, found)
	}
	if _, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 64}); found {
		t.Fatal("failed growth published the new entry")
	}
	if stats := module.vm.heap.stats(); stats.occupiedBytes != before.occupiedBytes || stats.reservedBytes != before.reservedBytes {
		t.Fatalf("failed growth changed ledger: before=%+v after=%+v", before, stats)
	}
}

func TestManagedMapGrowthFailureReleasesNestedStringForImmediateRetry(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, String>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key := int64(0); key < 64; key++ {
		if _, err := setIndexValue(module, value, newVMValue("Int", key), newVMValue("String", "value")); err != nil {
			t.Fatalf("insert %d: %v", key, err)
		}
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map did not materialize")
	}
	before := module.vm.heap.stats()
	stringLogical := ir.StorageRecordHeaderBytes + uint64(len("retry"))
	slabLogical := ir.StorageRecordHeaderBytes + uint64(managedMapSlabEntries)*
		(ir.StorageMapEntryBytes+managed.managed.keyLayout.Logical+managed.managed.valueLayout.Logical)
	module.vm.heap.maxGuestBytes = before.occupiedBytes + stringLogical + slabLogical
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(64)), newVMValue("String", "retry")); err == nil {
		t.Fatal("65th entry unexpectedly grew the map")
	}
	failed := module.vm.heap.stats()
	if failed.occupiedBytes != before.occupiedBytes || failed.reservedBytes != before.reservedBytes {
		t.Fatalf("failed growth retained nested backing: before=%+v after=%+v", before, failed)
	}
	if managed.managed.length() != 64 {
		t.Fatalf("failed growth changed map length: %d", managed.managed.length())
	}
	module.vm.heap.maxGuestBytes = 0
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(64)), newVMValue("String", "retry")); err != nil {
		t.Fatalf("immediate retry after failed growth: %v", err)
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 64})
	if !found || entry.Value.materializedData() != "retry" {
		t.Fatalf("retried map value = %#v, found=%t", entry, found)
	}
}

func TestManagedMapRetiredGenerationIsNeverReused(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, Int>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(1)), newVMValue("Int", int64(11))); err != nil {
		t.Fatal(err)
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map did not materialize")
	}
	header, err := managed.managed.readHeader()
	if err != nil {
		t.Fatal(err)
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 1})
	if !found {
		t.Fatal("initial entry missing")
	}
	slab, offset, err := managed.managed.location(header, uint32(entry.identity))
	if err != nil {
		t.Fatal(err)
	}
	if err := managed.managed.writeWord(slab, offset+4, ^uint32(0)); err != nil {
		t.Fatal(err)
	}
	if err := deleteValue(module, value, newVMValue("Int", int64(1))); err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(2)), newVMValue("Int", int64(22))); err != nil {
		t.Fatal(err)
	}
	newEntry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 2})
	if !found || uint32(newEntry.identity) == uint32(entry.identity) {
		t.Fatalf("retired entry was reused: old=%d new=%d", entry.identity, newEntry.identity)
	}
}

func TestManagedMapReferencesKeepYoungBackingAlive(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, Slice<Int>>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, value, newVMValue("Int", int64(1)), newOwnedSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(7))})); err != nil {
		t.Fatal(err)
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map did not materialize after collection")
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 1})
	if !found {
		t.Fatal("collected entry missing")
	}
	slice, ok := entry.Value.payload().(*vmSlice)
	if !ok || slice == nil || slice.valueAt(0).materializedData() != int64(7) {
		t.Fatalf("map value lost its backing after collection: %#v", entry.Value)
	}
}

func TestManagedMapMutationUsesCurrentWorkerBatch(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValueWithCapacityBatch(module, "Map<Int, Slice<Int>>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := module.vm.heap.acquireBatch(managedBatchCredit, managedBatchHandles)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValueWithBatch(module, value, newVMValue("Int", int64(1)), newOwnedSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(9))}), batch); err != nil {
		batch.abortUnfinished()
		_ = batch.flush()
		t.Fatal(err)
	}
	if batch.committed == 0 {
		batch.abortUnfinished()
		_ = batch.flush()
		t.Fatal("map mutation bypassed the current worker batch")
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
	managed, ok := materializeManagedMap(value)
	if !ok {
		t.Fatal("map did not materialize")
	}
	entry, found := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 1})
	if !found {
		t.Fatal("batch mutation did not publish map entry")
	}
	slice, ok := entry.Value.payload().(*vmSlice)
	if !ok || slice == nil || slice.valueAt(0).materializedData() != int64(9) {
		t.Fatalf("batch mutation produced invalid map value: %#v", entry.Value)
	}
}

func TestManagedWaitableResourceOwnsItsTypeClosure(t *testing.T) {
	module := newManagedValueTestModule()
	channel, err := makeWaitableValue(module, "Waitable<Array<2, Int>>", newVMValue("Int", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	resource, ok := materializeManagedWaitable(channel)
	if !ok || resource == nil {
		t.Fatal("waitable did not materialize")
	}
	if resource.Type.Table == channel.Type.Table {
		t.Fatal("managed waitable resource retained the source type table")
	}
	info, ok := resource.Type.WaitableInfo()
	wantInfo, wantOK := channel.Type.WaitableInfo()
	if !ok || !wantOK || info.Elem.String() != wantInfo.Elem.String() {
		t.Fatalf("owned waitable type = %s, info=%+v", resource.Type, info)
	}
	if resource.ElemType.Table == channel.Type.Table {
		t.Fatal("managed waitable element retained the source type table")
	}
}

func TestManagedWaitableRollsBackIdentityWhenBackingAdmissionFails(t *testing.T) {
	module := newManagedValueTestModule()
	// Waitable identity publication precedes the variable-size backing record.
	module.vm.heap.maxGuestBytes = 16
	if _, err := makeWaitableValue(module, "Waitable<Int>", newVMValue("Int", int64(2))); err == nil {
		t.Fatal("waitable construction unexpectedly succeeded past the guest budget")
	}
	stats := module.vm.heap.stats()
	if stats.occupiedBytes != 0 || stats.reservedBytes != 0 {
		t.Fatalf("failed waitable construction retained admission: occupied=%d reserved=%d", stats.occupiedBytes, stats.reservedBytes)
	}
}

func TestManagedWaitableClosePreservesHostStateWhenPageWriteFails(t *testing.T) {
	module := newManagedValueTestModule()
	channel, err := makeWaitableValue(module, "Waitable<Int>", newVMValue("Int", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	resource, err := waitableValueData(module, channel)
	if err != nil || resource == nil {
		t.Fatalf("waitable resource was not materialized: %v", err)
	}
	if err := module.vm.heap.release(channel.ref); err != nil {
		t.Fatal(err)
	}
	if err := waitableCloseValue(module, channel); err == nil {
		t.Fatal("close unexpectedly succeeded after identity release")
	}
	if resource.Closed {
		t.Fatal("host waitable state changed after managed close failed")
	}
}
