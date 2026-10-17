package runtime

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestOwnedStringSurvivesSourceFrameReuse(t *testing.T) {
	module := newManagedValueTestModule()
	typ := predeclaredRuntimeTypes["String"]
	function := loadedFunction{
		Decl: ir.Function{
			ID: "fn.string-transfer", Code: &ir.SlotCode{},
			Locals: []ir.Local{{ID: "value", Type: typ.Ref}},
		},
		LocalTypes: []vmType{typ}, LocalVariadic: []bool{false},
	}
	source, _, err := newFrame(module, function, []vmValue{newVMValue(typ, "kept")}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	owned := module.cloneValueForStore(source.localCells[0].load())
	source.recycle()
	destination, _, err := newFrame(module, function, []vmValue{owned}, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.recycle()
	if got := destination.localCells[0].load().payload(); got != "kept" {
		t.Fatalf("published string after source frame reuse = %#v", got)
	}
}

func TestDirectRendezvousPreservesInlineSliceAfterSourceFrameReuse(t *testing.T) {
	module := newManagedValueTestModule()
	typ := module.resolvedRuntimeType("Slice<Int>")
	function := transferFrameFunction("fn.slice-transfer", typ)
	source, _, err := newFrame(module, function, []vmValue{newOwnedSliceValue(typ, []vmValue{
		newVMValue("Int", int64(7)), newVMValue("Int", int64(11)),
	})}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	sent := source.localCells[0].load()
	received := directRendezvous(t, module, function, sent, source)
	slice, ok := received.payload().(*vmSlice)
	if !ok || slice == nil || slice.Len != 2 {
		t.Fatalf("received slice = %#v", received.payload())
	}
	for index, want := range []int64{7, 11} {
		if got, ok := slice.valueAt(index).signedValue(); !ok || got != want {
			t.Fatalf("received slice[%d] = %d, want %d", index, got, want)
		}
	}
}

func TestDirectRendezvousPreservesAnyAggregateAfterSourceFrameReuse(t *testing.T) {
	module := newManagedValueTestModule()
	typ := module.resolvedRuntimeType("Any")
	recordType := "struct{Items:Array<2, Int>}"
	function := transferFrameFunction("fn.any-transfer", typ)
	sourceValue := newVMValue(typ, newRuntimeStructValue(module, recordType, map[string]vmValue{
		"Items": newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(13)), newVMValue("Int", int64(17))}),
	}))
	source, _, err := newFrame(module, function, []vmValue{sourceValue}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	received := directRendezvous(t, module, function, source.localCells[0].load(), source)
	inner, ok := received.payload().(vmValue)
	if !ok {
		t.Fatalf("received Any payload = %#v", received.payload())
	}
	items, err := loadFieldValue(module, inner, "Items")
	if err != nil {
		t.Fatal(err)
	}
	array, ok := items.payload().(*vmArray)
	if !ok || array == nil || array.Len != 2 {
		t.Fatalf("received Any Items = %#v", items.payload())
	}
	for index, want := range []int64{13, 17} {
		if got, ok := array.valueAt(index).signedValue(); !ok || got != want {
			t.Fatalf("received Any Items[%d] = %d, want %d", index, got, want)
		}
	}
}

func transferFrameFunction(id string, typ vmType) loadedFunction {
	return loadedFunction{
		Decl:       ir.Function{ID: id, Code: &ir.SlotCode{}, Locals: []ir.Local{{ID: "value", Type: typ.Ref}}},
		LocalTypes: []vmType{typ}, LocalVariadic: []bool{false},
	}
}

func directRendezvous(t *testing.T, module *moduleInstance, function loadedFunction, value vmValue, source *frame) vmValue {
	t.Helper()
	channel, err := makeWaitableValue(module, "Waitable<"+function.LocalTypes[0].String()+">", newVMValue("Int", int64(0)))
	if err != nil {
		t.Fatal(err)
	}
	sender, err := module.vm.prepareChannelSelection(nil, module, []channelSelectCase{{channel: channel, send: true, value: value}})
	if err != nil {
		t.Fatal(err)
	}
	sender.register()
	receiver, err := module.vm.prepareChannelSelection(nil, module, []channelSelectCase{{channel: channel}})
	if err != nil {
		t.Fatal(err)
	}
	if !receiver.tryCommit() || !receiver.ok || !sender.done {
		t.Fatalf("direct rendezvous: received=%t senderDone=%t", receiver.ok, sender.done)
	}
	// Reuse the producer activation before publishing the selected value into
	// the receiver. This is the lifetime boundary that a blocked receiver sees.
	source.recycle()
	destination, _, err := newFrame(module, function, nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	destination.slotOperands = &ir.SlotOperands{Outputs: []uint32{ir.LocalOutput}}
	if err := receiver.deliver(destination, nil, false); err != nil {
		destination.recycle()
		t.Fatal(err)
	}
	value = destination.localCells[0].load()
	destination.recycle()
	return value
}
