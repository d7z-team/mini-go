package runtime

import (
	"fmt"
	goruntime "runtime"
	"strconv"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPointerCensusCountsSharedByteStorage(t *testing.T) {
	module := &moduleInstance{}
	for _, size := range []int{1024, 65536, 1 << 20} {
		source := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, size), size, size)
		pointer, ok, err := module.convertSliceToArrayPointer(source, runtimeTypeFromText(fmt.Sprintf("Ptr<Array<%d, Uint8>>", size)))
		if !ok || err != nil {
			t.Fatalf("convert: %v %v", ok, err)
		}
		var before, after goruntime.MemStats
		goruntime.ReadMemStats(&before)
		sizer := newRuntimeValueSizer()
		sizer.value(pointer)
		goruntime.ReadMemStats(&after)
		if want := 2*ir.RuntimeNodeBytes + int64(size)*ir.RuntimeByteBytes; sizer.bytes != want {
			t.Fatalf("backing=%d census=%d want=%d", size, sizer.bytes, want)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
			t.Fatalf("inspection expanded %d bytes into %d allocated bytes", size, allocated)
		}
		previous := sizer.bytes
		sizer.value(source)
		sizer.value(pointer)
		if sizer.bytes != previous {
			t.Fatal("shared storage counted twice")
		}
	}
}

func TestPointerInspectionPreservesLazyStateAndFieldSelection(t *testing.T) {
	module := &moduleInstance{}
	lazy := newSlot(runtimeTypeFromText("Array<4096, Uint8>"), module, false)
	walker := newRuntimeValueWalker(func(*instanceRevision) { t.Fatal("unexpected revision") })
	walker.slot(lazy)
	walker.value(newSlotPointerValue(lazy.typ, "lazy", lazy))
	if lazy.initialized || lazy.value.Data != nil {
		t.Fatal("inspection initialized a lazy value")
	}
	revision := &instanceRevision{generation: 1}
	owner := &moduleInstance{revision: revision}
	parent := newRuntimeStructValue(nil, "record", map[string]vmValue{
		"Callback": newVMValue("Function", functionRef{exact: owner}),
		"Count":    newVMValue("Int", int64(7)),
	})
	cell := reflectCellPointer(module, "record", "record", parent)
	for _, field := range []string{"Callback", "Count"} {
		pointer := embeddedFieldPointer(module, cell, field, "Any")
		found := false
		newRuntimeValueWalker(func(r *instanceRevision) { found = r == revision }).value(pointer)
		if found != (field == "Callback") {
			t.Fatalf("field %s: revision=%v", field, found)
		}
	}
	found := false
	walker = newRuntimeValueWalker(func(r *instanceRevision) { found = found || r == revision })
	walker.value(embeddedFieldPointer(module, cell, "Count", "Any"))
	if found {
		t.Fatal("field inspection followed an unrelated callback")
	}
	walker.value(cell)
	if !found {
		t.Fatal("address traversal hid the parent callback from a later root")
	}
}

func TestPointerInspectionBoundsNestedAddresses(t *testing.T) {
	value := newVMValue("Int", int64(1))
	for range 1000 {
		parent := newRuntimeStructValue(nil, "record", map[string]vmValue{"Value": value})
		value = embeddedFieldPointer(&moduleInstance{}, parent, "Value", "Any")
	}
	walker := newRuntimeValueWalker(func(*instanceRevision) { t.Fatal("unexpected revision") })
	nodes := 0
	walker.enter = func(vmValue) bool {
		if nodes == 4 {
			walker.stopped = true
			return false
		}
		nodes++
		return true
	}
	walker.leave = func() {}
	walker.value(value)
	if !walker.stopped || nodes != 4 {
		t.Fatalf("inspection budget: nodes=%d stopped=%v", nodes, walker.stopped)
	}
}

func TestArrayPointerCensusKeepsBackingOutsideItsView(t *testing.T) {
	source := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, 128), 128, 128)
	view := newSliceViewValue("Slice<Uint8>", source.Data.(*vmSlice), 32, 64, 96)
	module := &moduleInstance{}
	pointer, ok, err := module.convertSliceToArrayPointer(view, runtimeTypeFromText("Ptr<Array<8, Uint8>>"))
	if !ok || err != nil {
		t.Fatalf("conversion: %v %v", ok, err)
	}
	sizer := newRuntimeValueSizer()
	sizer.value(pointer)
	if want := int64(2*ir.RuntimeNodeBytes + 128*ir.RuntimeByteBytes); sizer.bytes != want {
		t.Fatalf("view census=%d want=%d", sizer.bytes, want)
	}
	sizer.value(source)
	if want := int64(3*ir.RuntimeNodeBytes + 128*ir.RuntimeByteBytes); sizer.bytes != want {
		t.Fatalf("shared view census=%d want=%d", sizer.bytes, want)
	}
}

func BenchmarkByteArrayPointerCensus(b *testing.B) {
	for _, size := range []int{1024, 65536, 1 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			module := &moduleInstance{}
			source := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, size), size, size)
			pointer, _, err := module.convertSliceToArrayPointer(source, runtimeTypeFromText(fmt.Sprintf("Ptr<Array<%d, Uint8>>", size)))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				newRuntimeValueSizer().value(pointer)
			}
		})
	}
}
