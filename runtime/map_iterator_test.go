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
	f := &frame{module: module}
	if err = f.initMapIterator("range", object); err != nil {
		t.Fatal(err)
	}
	sum := int64(0)
	for range 3 {
		if err = f.nextMapIterator("range"); err != nil {
			t.Fatal(err)
		}
		values, err := f.popN(3)
		if err != nil {
			t.Fatal(err)
		}
		if values[2].Data == true {
			v, _ := values[1].signedValue()
			sum += v
		}
	}
	if sum != 42 {
		t.Fatalf("NaN entry values = %d", sum)
	}
	if err = f.initMapIterator("range", object); err != nil {
		t.Fatal(err)
	}
	if err = clearValue(module, object); err != nil {
		t.Fatal(err)
	}
	if _, err = setIndexValue(module, object, newVMValue("Float64", float64(1)), newVMValue("Int", int64(99))); err != nil {
		t.Fatal(err)
	}
	if err = f.nextMapIterator("range"); err != nil {
		t.Fatal(err)
	}
	values, _ := f.popN(3)
	if values[2].Data != false {
		t.Fatal("visited entry inserted after iterator initialization")
	}
	f.recycle()
	if len(f.mapIterators) != 0 {
		t.Fatal("recycled frame retained map cursor")
	}
}

func TestMapIteratorAllocationFailureAndUninitializedAccess(t *testing.T) {
	machine := &vm{limits: normalizeLimits(Limits{MaxAllocatedBytes: 1})}
	f := &frame{module: &moduleInstance{vm: machine}}
	if err := f.nextMapIterator("missing"); err == nil {
		t.Fatal("missing cursor must fail")
	}
	err := f.initMapIterator("range", newVMValue("Map<Int, Int>", nil))
	var limit ResourceLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("allocation error = %v", err)
	}
	if len(f.mapIterators) != 0 {
		t.Fatal("failed initialization published iterator")
	}
}

func TestMapIteratorSkipsReinsertedKeyAndObservesUpdates(t *testing.T) {
	module := &moduleInstance{vm: &vm{limits: normalizeLimits(Limits{})}}
	object, err := newMapValue(module, "Map<Int, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(10)), newVMValue("Int", int64(2)), newVMValue("Int", int64(20))})
	if err != nil {
		t.Fatal(err)
	}
	f := &frame{module: module}
	if err = f.initMapIterator("range", object); err != nil {
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
	if err = f.nextMapIterator("range"); err != nil {
		t.Fatal(err)
	}
	values, _ := f.popN(3)
	requireValues(t, values, newVMValue("Int", int64(2)), newVMValue("Int", int64(42)), newBoolValue(true))
	if err = f.nextMapIterator("range"); err != nil {
		t.Fatal(err)
	}
	values, _ = f.popN(3)
	if values[2].Data != false {
		t.Fatal("reinserted key visited")
	}
	if err = module.vm.executeInstruction(nil, f, &preparedInstruction{op: preparedMapIterClose, local: &ir.LocalPayload{Local: "range"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.mapIterators) != 0 {
		t.Fatal("closed iterator retained state")
	}
}
