package runtime

import (
	"sync"
	"testing"
)

func TestStructStorageConcurrentFieldsAndValueCopies(t *testing.T) {
	value := newRuntimeStructValue(nil, "struct{A:Int,B:Int}", map[string]vmValue{
		"A": newVMValue("Int", int64(0)), "B": newVMValue("Int", int64(0)),
	})
	storage := value.Data.(*vmStruct)
	var module *moduleInstance
	before := module.cloneValueForStore(value)
	var group sync.WaitGroup
	for _, name := range []string{"A", "B"} {
		group.Go(func() {
			for n := int64(1); n <= 100; n++ {
				if _, ok := updatedStructValue(storage, name, newVMValue("Int", n)); !ok {
					t.Error("field update failed")
				}
				copied := module.cloneValueForStore(value)
				fields, ok := materializeStructValue(copied.Data)
				if !ok || len(fields) != 2 {
					t.Errorf("invalid copied fields: %v", fields)
				}
			}
		})
	}
	group.Wait()
	for _, name := range []string{"A", "B"} {
		old, _ := structValueField(before.Data, name)
		current, _ := structValueField(storage, name)
		if old.materializedData() != int64(0) || current.materializedData() != int64(100) {
			t.Fatalf("field %s: old=%v current=%v", name, old, current)
		}
	}
	assigned := module.assignPreparedValue(value, before)
	if assigned.Data != storage {
		t.Fatal("whole assignment changed addressable struct storage")
	}
	for _, name := range []string{"A", "B"} {
		current, _ := structValueField(storage, name)
		if current.materializedData() != int64(0) {
			t.Fatalf("whole assignment did not replace field %s: %v", name, current)
		}
	}
}

func TestConcurrentZeroArrayFieldReadsShareAddressableStorage(t *testing.T) {
	module := &moduleInstance{}
	value := module.zeroValue("struct{Items:Array<2, Int>}")
	results := make([]*vmArray, 4)
	var group sync.WaitGroup
	for index := range results {
		group.Go(func() {
			field, err := loadFieldValue(module, value, "Items")
			if err != nil {
				t.Error(err)
				return
			}
			results[index] = field.Data.(*vmArray)
		})
	}
	group.Wait()
	for _, result := range results {
		if result == nil || result.vmSliceStorage != results[0].vmSliceStorage {
			t.Fatal("zero field reads materialized independent backing")
		}
	}
	if err := results[0].setValueAt(1, newVMValue("Int", int64(42))); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.valueAt(1).materializedData() != int64(42) {
			t.Fatal("array field alias lost a write")
		}
	}
	module.assignPreparedValue(value, module.zeroValue(value.Type))
	for _, result := range results {
		if result.valueAt(1).materializedData() != int64(0) {
			t.Fatal("zero struct assignment detached the array field backing")
		}
	}
}
