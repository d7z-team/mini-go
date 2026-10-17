package runtime

import (
	"encoding/json"
	"os"
	"testing"
)

// This shared vector measures established storage, excluding the captured
// cell that already belongs to the caller. Physical descriptors may differ
// across backends, but logical allocation units do not.
func TestManagedStorageAllocationCosts(t *testing.T) {
	raw, err := os.ReadFile("../testdata/runtime/storage_cost.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name        string `json:"name"`
		Bytes       uint64 `json:"bytes"`
		Allocations uint64 `json:"allocations"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			module := newManagedValueTestModule()
			capture := newSlot(coerceRuntimeType("Int"), module, false)
			capture.publish(newVMValue("Int", int64(42)))
			if _, err := capture.ensureManagedCell(); err != nil {
				t.Fatal(err)
			}
			before := module.vm.heap.counters
			var value vmValue
			switch vector.Name {
			case "bool_cell":
				value = newVMValue("Bool", true)
			case "int_cell":
				value = newVMValue("Int", int64(42))
			case "empty_array_cell":
				value = newVMValue("Array<0, Int>", []vmValue{})
			case "string_cell":
				value = newVMValue("String", "abc")
			case "byte_slice_backing", "int_slice_backing":
				backing := newSliceValue("Slice<Int>", []vmValue{
					newVMValue("Int", int64(1)), newVMValue("Int", int64(2)), newVMValue("Int", int64(3)),
				})
				if vector.Name == "byte_slice_backing" {
					backing = newByteSliceHeaderValue("Slice<Uint8>", []byte{1, 2, 3}, 3, 3)
				}
				var descriptor [32]byte
				err = module.initializeSliceValueWithBatch(descriptor[:], backing.Type, backing.Data.(*vmSlice), map[uintptr]guestRef{}, nil)
			case "function", "closure":
				function := functionRef{ModulePath: "main", FunctionID: "fn.entry"}
				if vector.Name == "closure" {
					function.captureSlots = []*slot{capture}
				}
				_, err = module.newManagedFunction(coerceRuntimeType("Function"), function, nil)
			case "bound_method":
				_, err = module.newManagedFunction(coerceRuntimeType("Function"), reflectMethodTarget{Receiver: newVMValue("Int", int64(42))}, nil)
			case "rendezvous_channel", "buffered_channel":
				capacity := 0
				if vector.Name == "buffered_channel" {
					capacity = 2
				}
				_, err = module.newManagedWaitableWithBatch(coerceRuntimeType("Waitable<Int>"), coerceRuntimeType("Int"), capacity, nil)
			case "empty_map", "populated_map":
				capacity := 0
				if vector.Name == "populated_map" {
					capacity = 1
				}
				_, err = module.newManagedMapWithBatch(coerceRuntimeType("Map<Int, Int>"), nil, capacity, nil)
			default:
				t.Fatalf("unknown vector %q", vector.Name)
			}
			if value.Type.Valid() {
				_, err = module.newManagedCell(value.Type, value)
			}
			if err != nil {
				t.Fatal(err)
			}
			after := module.vm.heap.counters
			if got := after.totalAllocatedBytes - before.totalAllocatedBytes; got != vector.Bytes {
				t.Errorf("allocated bytes = %d, want %d", got, vector.Bytes)
			}
			if got := after.allocations - before.allocations; got != vector.Allocations {
				t.Errorf("allocations = %d, want %d", got, vector.Allocations)
			}
		})
	}
}
