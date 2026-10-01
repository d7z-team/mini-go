package runtime

import (
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestAppendCapacityFailurePreservesBackingAndAccounting(t *testing.T) {
	for _, element := range []string{"Int", "Uint8"} {
		t.Run(element, func(t *testing.T) {
			machine := &vm{limits: Limits{MaxCollectionElements: 3}}
			module := &moduleInstance{vm: machine}
			value := newVMValue(element, int64(7))
			slice := newSliceHeaderValue("Slice<"+element+">", []vmValue{value, module.zeroValue(element)}, 0, 1, 2)
			if element == "Uint8" {
				value = newVMValue(element, uint64(7))
				slice = newByteSliceHeaderValue("Slice<Uint8>", []byte{7, 0}, 1, 2)
			}
			_, err := appendValue(module, slice, []vmValue{value, value}, false)
			var limit ResourceLimitError
			if !errors.As(err, &limit) || limit.Code != "execution.collection_limit" {
				t.Fatalf("growth failure: %v", err)
			}
			header := slice.Data.(*vmSlice)
			if header.Len != 1 || header.Cap != 2 || machine.allocatedSinceSweep.Load() != 0 {
				t.Fatal("rejected growth changed header or accounting")
			}
			machine.limits.MaxCollectionElements = 4
			result, err := appendValue(module, slice, []vmValue{value, value}, false)
			if err != nil {
				t.Fatal(err)
			}
			grown := result.Data.(*vmSlice)
			if grown.Len != 3 || grown.Cap != 4 || header.Len != 1 || header.Cap != 2 {
				t.Fatal("growth changed the original slice header")
			}
			if got := machine.allocatedSinceSweep.Load(); got != 2*ir.RuntimeSlotBytes {
				t.Fatalf("logical growth charge = %d, want %d", got, 2*ir.RuntimeSlotBytes)
			}
		})
	}
}
