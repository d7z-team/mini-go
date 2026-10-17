package runtime

import (
	"errors"
	"testing"

	"github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestManagedWaitableCapacityUsesElementStorageCost(t *testing.T) {
	for _, test := range []struct {
		typ   string
		width uint64
	}{
		{"Bool", 1},
		{"Int16", 2},
		{"Int", 8},
		{"String", 24},
		{"Array<3, Int>", 24},
		{"struct{Small:Bool,Wide:Int}", 9},
	} {
		t.Run(test.typ, func(t *testing.T) {
			module := newManagedValueTestModule()
			module.vm.heap.maxGuestBytes = 2*bytecode.StorageRecordHeaderBytes + bytecode.StorageChannelHeaderBytes + 5*test.width
			channel, err := makeWaitableValue(module, "Waitable<"+test.typ+">", newVMValue("Int", int64(5)))
			if err != nil {
				t.Fatal(err)
			}
			if got := module.vm.heap.stats().occupiedBytes; got != module.vm.heap.maxGuestBytes {
				t.Fatalf("channel storage = %d, want %d", got, module.vm.heap.maxGuestBytes)
			}
			if _, err := module.vm.heap.collect([]guestRef{channel.ref}, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedWaitableRingCopiesValuesAndTracesWrappedReferences(t *testing.T) {
	module := newManagedValueTestModule()
	const elem = "struct{Items:Array<2, Int>,Shared:Slice<Int>}"
	channel, err := makeWaitableValue(module, "Waitable<"+elem+">", newVMValue("Int", int64(2)))
	if err != nil {
		t.Fatal(err)
	}
	resource, err := waitableValueData(module, channel)
	if err != nil {
		t.Fatal(err)
	}
	module.vm.heap.poisonCollected = true
	for range 2 {
		if _, err := module.vm.heap.collect([]guestRef{channel.ref}, false); err != nil {
			t.Fatal(err)
		}
	}
	source := module.adoptValue(newRuntimeStructValue(module, elem, map[string]vmValue{
		"Items":  newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(7)), newVMValue("Int", int64(8))}),
		"Shared": newOwnedSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(11))}),
	}))
	if err := module.vm.takeAllocationFailure(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := resource.appendBuffer(source); err != nil {
			t.Fatal(err)
		}
	}
	items, err := loadFieldValue(module, source, "Items")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, items, newVMValue("Int", int64(0)), newVMValue("Int", int64(99))); err != nil {
		t.Fatal(err)
	}
	first, err := resource.popBuffer()
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the first ring slot while the received value remains live.
	if err := resource.appendBuffer(source); err != nil {
		t.Fatal(err)
	}
	if _, err := module.vm.heap.collect([]guestRef{channel.ref, first.ref}, false); err != nil {
		t.Fatal(err)
	}
	firstItems, err := loadFieldValue(module, first, "Items")
	if err != nil || firstItems.payload().(*vmArray).valueAt(0).materializedData() != int64(7) {
		t.Fatalf("received value was overwritten: %v", err)
	}
	shared, err := loadFieldValue(module, first, "Shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, shared, newVMValue("Int", int64(0)), newVMValue("Int", int64(13))); err != nil {
		t.Fatal(err)
	}
	// Only the channel remains a root; buffered aggregate references keep the
	// shared slice backing alive, independently of the source/received records.
	if _, err := module.vm.heap.collect([]guestRef{channel.ref}, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{7, 99} {
		value, err := resource.popBuffer()
		if err != nil {
			t.Fatal(err)
		}
		items, err := loadFieldValue(module, value, "Items")
		if err != nil || items.payload().(*vmArray).valueAt(0).materializedData() != want {
			t.Fatalf("ring value = %v, want %d: %v", items, want, err)
		}
		shared, err := loadFieldValue(module, value, "Shared")
		if err != nil || shared.payload().(*vmSlice).valueAt(0).materializedData() != int64(13) {
			t.Fatalf("shared slice lost: %v", err)
		}
	}
}

func TestManagedWaitableReceiveAllocationFailurePreservesBuffer(t *testing.T) {
	module := newManagedValueTestModule()
	channel, err := makeWaitableValue(module, "Waitable<Array<2, Int>>", newVMValue("Int", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	resource, err := waitableValueData(module, channel)
	if err != nil {
		t.Fatal(err)
	}
	value := newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(7)), newVMValue("Int", int64(8))})
	if err := resource.appendBuffer(value); err != nil {
		t.Fatal(err)
	}
	before := module.vm.heap.stats()
	module.vm.heap.maxGuestBytes = before.occupiedBytes
	if _, err := resource.popBuffer(); !errors.Is(err, errManagedCollection) {
		t.Fatalf("receive error = %v", err)
	}
	if resource.bufferLen() != 1 || module.vm.heap.stats().reservedBytes != 0 {
		t.Fatal("failed receive consumed buffer or leaked reservation")
	}
	module.vm.heap.maxGuestBytes = 0
	got, err := resource.popBuffer()
	if err != nil || got.payload().(*vmArray).valueAt(0).materializedData() != int64(7) {
		t.Fatalf("retry value = %v: %v", got, err)
	}
}
