package runtime

import "testing"

func TestCompositeConstructionPublishesThroughAllocationBatch(t *testing.T) {
	module := newManagedValueTestModule()
	batch, err := module.vm.heap.acquireBatch(managedBatchCredit, managedBatchHandles)
	if err != nil {
		t.Fatal(err)
	}
	value, err := newSequenceValueWithBatch(module, "Slice<Int>", []vmValue{
		newVMValue("Int", int64(3)),
		newVMValue("Int", int64(5)),
	}, batch)
	if err != nil {
		t.Fatalf("sequence construction: %v", err)
	}
	if !value.ref.valid() || value.heap != module.vm.heap {
		t.Fatalf("sequence was not published through the batch: %#v", value)
	}
	if batch.committed == 0 {
		t.Fatal("sequence construction did not consume batch credit")
	}
	values, ok := value.payload().(*vmSlice)
	if !ok || values == nil {
		t.Fatalf("sequence payload = %#v", value.payload())
	}
	if got, ok := values.valueAt(1).signedValue(); !ok || got != 5 {
		t.Fatalf("sequence value = %v, %v", got, ok)
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendConstructionPublishesThroughAllocationBatch(t *testing.T) {
	module := newManagedValueTestModule()
	base := module.adoptValue(newSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(1))}))
	batch, err := module.vm.heap.acquireBatch(managedBatchCredit, managedBatchHandles)
	if err != nil {
		t.Fatal(err)
	}
	value, err := appendValueWithBatch(module, base, []vmValue{newVMValue("Int", int64(7))}, false, batch)
	if err != nil {
		t.Fatalf("append construction: %v", err)
	}
	if !value.ref.valid() || value.heap != module.vm.heap {
		t.Fatalf("append was not published through the batch: %#v", value)
	}
	values, ok := value.payload().(*vmSlice)
	if !ok || values == nil {
		t.Fatalf("append payload = %#v", value.payload())
	}
	if got, ok := values.valueAt(1).signedValue(); !ok || got != 7 {
		t.Fatalf("append value = %v, %v", got, ok)
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedMapConstructionRollsBackAdmissionOnStorageFailure(t *testing.T) {
	module := newManagedValueTestModule()
	module.vm.heap.maxResidentBytes = 1
	_, err := newMapValueWithCapacityBatch(module, "Map<Int,Int>", []vmValue{
		newVMValue("Int", int64(1)),
		newVMValue("Int", int64(2)),
	}, 1, nil)
	if err == nil {
		t.Fatal("map construction unexpectedly succeeded with no resident budget")
	}
	if got := module.vm.heap.stats().occupiedBytes; got != 0 {
		t.Fatalf("failed map construction retained admission bytes: %d", got)
	}
	if got := module.vm.heap.stats().reservedBytes; got != 0 {
		t.Fatalf("failed map construction retained reserved bytes: %d", got)
	}
}

func TestManagedPublicationRecordsStorageFailureWithoutPanic(t *testing.T) {
	module := newManagedValueTestModule()
	batch, err := module.vm.heap.acquireBatch(managedBatchCredit, managedBatchHandles)
	if err != nil {
		t.Fatal(err)
	}
	module.vm.heap.maxResidentBytes = module.vm.heap.counters.residentPageBytes
	value := module.adoptValueWithBatch(newSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(1))}), batch)
	if value.ref.valid() {
		t.Fatalf("failed publication returned a managed reference: %#v", value)
	}
	if batch.failure == nil {
		t.Fatal("failed publication did not record its allocation error")
	}
	if err := batch.flush(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPublicationWithoutBatchRecordsStorageFailureWithoutPanic(t *testing.T) {
	module := newManagedValueTestModule()
	module.vm.heap.maxResidentBytes = 1
	value := module.adoptValue(newSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(1))}))
	if value.ref.valid() {
		t.Fatalf("failed publication returned a managed reference: %#v", value)
	}
	if err := module.vm.takeAllocationFailure(); err == nil {
		t.Fatal("failed publication did not record its allocation error")
	}
}
