package runtime

import "testing"

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
