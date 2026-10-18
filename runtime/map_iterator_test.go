package runtime

import (
	"errors"
	"math"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestMapIteratorTracksEntriesAcrossMutation(t *testing.T) {
	module := &moduleInstance{vm: &vm{limits: normalizeLimits(Limits{})}}
	object, err := newMapValue(module, "Map<Float64, Int>", []vmValue{
		newVMValue("Float64", math.NaN()), newVMValue("Int", int64(20)),
		newVMValue("Float64", math.NaN()), newVMValue("Int", int64(22)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &frame{module: module, mapIteratorSlots: make([]*mapIterator, 1), slotValues: make([]vmValue, 3), slotOperands: &ir.RegisterFlow{Outputs: []uint32{0, 1, 2}}}
	if err = f.initMapIteratorAt(0, "range", object); err != nil {
		t.Fatal(err)
	}
	sum := int64(0)
	for range 3 {
		f.slotOutput = 0
		if err = f.nextMapIteratorAt(0, "range"); err != nil {
			t.Fatal(err)
		}
		values := f.slotValues
		if values[2].Data == true {
			v, _ := values[1].signedValue()
			sum += v
		}
	}
	if sum != 42 {
		t.Fatalf("NaN entry values = %d", sum)
	}
	if err = f.initMapIteratorAt(0, "range", object); err != nil {
		t.Fatal(err)
	}
	if err = clearValue(module, object); err != nil {
		t.Fatal(err)
	}
	if _, err = setIndexValue(module, object, newVMValue("Float64", float64(1)), newVMValue("Int", int64(99))); err != nil {
		t.Fatal(err)
	}
	f.slotOutput = 0
	if err = f.nextMapIteratorAt(0, "range"); err != nil {
		t.Fatal(err)
	}
	values := f.slotValues
	if values[2].Data != false {
		t.Fatal("visited entry inserted after iterator initialization")
	}
	f.recycle()
	if f.mapIteratorSlots[0] != nil {
		t.Fatal("recycled frame retained map cursor")
	}
}

func TestPreparedMapIteratorUsesIndexedCursor(t *testing.T) {
	module := &moduleInstance{vm: &vm{limits: normalizeLimits(Limits{})}}
	object, err := newMapValue(module, "Map<Int, Int>", []vmValue{
		newVMValue("Int", int64(7)), newVMValue("Int", int64(42)),
		newVMValue("Int", int64(8)), newVMValue("Int", int64(84)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &frame{
		module:           module,
		localStorage:     make([]slot, 1),
		localCells:       []*slot{newSlot(predeclaredRuntimeTypes["Int"], module, false)},
		mapIteratorSlots: make([]*mapIterator, 1),
		slotValues:       make([]vmValue, 3),
		slotOperands:     &ir.RegisterFlow{Outputs: []uint32{0, 1, 2}},
	}
	if err := f.initMapIteratorAt(0, "range", object); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][2]int64{{7, 42}, {8, 84}} {
		f.slotOutput = 0
		if err := f.nextMapIteratorAt(0, "range"); err != nil {
			t.Fatal(err)
		}
		if got := f.slotValues[0].materializedData(); got != want[0] {
			t.Fatalf("indexed key = %#v, want %d", got, want[0])
		}
		if got := f.slotValues[1].materializedData(); got != want[1] {
			t.Fatalf("indexed value = %#v, want %d", got, want[1])
		}
		if f.slotValues[2].Data != true {
			t.Fatal("indexed iterator did not publish success")
		}
	}
	f.slotOutput = 0
	if err := f.nextMapIteratorAt(0, "range"); err != nil {
		t.Fatal(err)
	}
	if f.slotValues[2].Data != false || f.mapIteratorSlots[0].admittedBytes != 0 {
		t.Fatal("indexed iterator did not release at end")
	}
	if err := f.initMapIteratorAt(0, "range", object); err != nil {
		t.Fatal(err)
	}
	iterator := f.mapIteratorSlots[0]
	f.recycle()
	if f.mapIteratorSlots != nil && f.mapIteratorSlots[0] != nil {
		t.Fatal("recycled frame retained indexed cursor")
	}
	if iterator.admittedBytes != 0 || iterator.residentBytes != 0 {
		t.Fatal("recycled frame retained iterator reservation")
	}
}

func TestMapIteratorAllocationFailureAndUninitializedAccess(t *testing.T) {
	machine := &vm{limits: normalizeLimits(Limits{MaxGuestBytes: 1})}
	f := &frame{module: &moduleInstance{vm: machine}}
	if err := f.nextMapIteratorAt(0, "missing"); err == nil {
		t.Fatal("missing cursor must fail")
	}
	f.mapIteratorSlots = make([]*mapIterator, 1)
	err := f.initMapIteratorAt(0, "range", newVMValue("Map<Int, Int>", nil))
	var limit ResourceLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("allocation error = %v", err)
	}
	if f.mapIteratorSlots[0] != nil {
		t.Fatal("failed initialization published iterator")
	}
}

func TestMapIteratorReplacementAdmissionFailureKeepsPreviousOwner(t *testing.T) {
	module := newManagedValueTestModule()
	object, err := newMapValue(module, "Map<Int, Int>", []vmValue{newVMValue("Int", int64(7)), newVMValue("Int", int64(42))})
	if err != nil {
		t.Fatal(err)
	}
	f := &frame{module: module, mapIteratorSlots: make([]*mapIterator, 1), slotValues: make([]vmValue, 3), slotOperands: &ir.RegisterFlow{Outputs: []uint32{0, 1, 2}}}
	defer f.recycle()
	if err := f.initMapIteratorAt(0, "range", object); err != nil {
		t.Fatal(err)
	}
	previous := f.mapIteratorSlots[0]
	before := module.vm.heap.stats()
	module.vm.heap.maxGuestBytes = before.occupiedBytes
	if err := f.initMapIteratorAt(0, "range", object); err == nil {
		t.Fatal("replacement unexpectedly exceeded its owner budget")
	}
	if f.mapIteratorSlots[0] != previous || previous.admittedBytes != ir.StorageControlRecordBytes+ir.StorageReferenceBytes {
		t.Fatal("failed replacement changed the previous iterator owner")
	}
	after := module.vm.heap.stats()
	if after.occupiedBytes != before.occupiedBytes || after.reservedBytes != before.reservedBytes || after.residentPageBytes != before.residentPageBytes {
		t.Fatalf("failed replacement changed admission: before=%+v after=%+v", before, after)
	}
	module.vm.heap.maxGuestBytes = 0
	module.vm.heap.maxResidentBytes = before.residentPageBytes
	if err := f.initMapIteratorAt(0, "range", object); !errors.Is(err, errManagedLimit) {
		t.Fatalf("resident admission error = %v", err)
	}
	after = module.vm.heap.stats()
	if after.occupiedBytes != before.occupiedBytes || after.reservedBytes != before.reservedBytes || after.residentPageBytes != before.residentPageBytes || after.totalAllocatedBytes != before.totalAllocatedBytes {
		t.Fatalf("resident rejection changed committed ownership: before=%+v after=%+v", before, after)
	}
	if err := f.nextMapIteratorAt(0, "range"); err != nil {
		t.Fatal(err)
	}
	if value, ok := f.slotValues[1].signedValue(); !ok || value != 42 {
		t.Fatalf("previous iterator lost its value: %+v", f.slotValues)
	}
}

func TestMapIteratorSkipsReinsertedKeyAndObservesUpdates(t *testing.T) {
	module := &moduleInstance{vm: &vm{limits: normalizeLimits(Limits{})}}
	object, err := newMapValue(module, "Map<Int, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(10)), newVMValue("Int", int64(2)), newVMValue("Int", int64(20))})
	if err != nil {
		t.Fatal(err)
	}
	f := &frame{module: module, mapIteratorSlots: make([]*mapIterator, 1), slotValues: make([]vmValue, 3), slotOperands: &ir.RegisterFlow{Outputs: []uint32{0, 1, 2}}}
	if err = f.initMapIteratorAt(0, "range", object); err != nil {
		t.Fatal(err)
	}
	if err = deleteValue(module, object, newVMValue("Int", int64(1))); err != nil {
		t.Fatal(err)
	}
	for _, key := range []int64{1, 2} {
		if _, err = setIndexValue(module, object, newVMValue("Int", key), newVMValue("Int", int64(42))); err != nil {
			t.Fatal(err)
		}
	}
	f.slotOutput = 0
	if err = f.nextMapIteratorAt(0, "range"); err != nil {
		t.Fatal(err)
	}
	values := f.slotValues
	requireValues(t, values, newVMValue("Int", int64(2)), newVMValue("Int", int64(42)), newBoolValue(true))
	f.slotOutput = 0
	if err = f.nextMapIteratorAt(0, "range"); err != nil {
		t.Fatal(err)
	}
	values = f.slotValues
	if values[2].Data != false {
		t.Fatal("reinserted key visited")
	}
	if err = module.vm.executeInstruction(nil, f, &preparedInstruction{op: preparedMapIterClose, local: &ir.LocalPayload{Local: "range"}}); err != nil {
		t.Fatal(err)
	}
	if f.mapIteratorSlots[0] != nil {
		t.Fatal("closed iterator retained state")
	}
}
