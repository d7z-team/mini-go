package runtime

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func newManagedValueTestModule() *moduleInstance {
	machine := &vm{heap: newManagedHeap(), valueLayouts: newPreparedLayoutRegistry()}
	machine.valueLayouts.heap = machine.heap
	machine.valueLayouts.owner = machine
	machine.heap.onType = machine.valueLayouts.mark
	machine.heap.onMajor = machine.reclaimManagedLayouts
	machine.heap.onReclaim = machine.reclaimManagedMetadata
	return &moduleInstance{vm: machine}
}

func TestManagedAggregateCopySeparatesValuesAndRetainsReferenceAliases(t *testing.T) {
	for _, sourceKind := range []string{"record", "record-view", "frame-view"} {
		t.Run(sourceKind, func(t *testing.T) {
			module := newManagedValueTestModule()
			original := module.adoptValue(newRuntimeStructValue(module, "struct{Items:Array<2, Int>,Shared:Slice<Int>}", map[string]vmValue{
				"Items":  newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2))}),
				"Shared": newOwnedSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(9))}),
			}))
			if err := module.vm.takeAllocationFailure(); err != nil {
				t.Fatal(err)
			}
			source := original
			switch sourceKind {
			case "record-view":
				source = newVMValue(original.Type, original.payload())
			case "frame-view":
				layout, err := module.vm.valueLayouts.layoutFor(original.Type)
				if err != nil {
					t.Fatal(err)
				}
				raw := make([]byte, layout.Stride)
				if err := module.vm.heap.read(original.ref, original.offset, raw); err != nil {
					t.Fatal(err)
				}
				source = readInlineAggregateOwned(module, original.Type, layout, raw, nil)
			}
			copied := module.cloneValueForStore(source)
			if err := module.vm.takeAllocationFailure(); err != nil {
				t.Fatal(err)
			}
			items, err := loadFieldValue(module, source, "Items")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := setIndexValue(module, items, newVMValue("Int", int64(0)), newVMValue("Int", int64(7))); err != nil {
				t.Fatal(err)
			}
			shared, err := loadFieldValue(module, copied, "Shared")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := setIndexValue(module, shared, newVMValue("Int", int64(0)), newVMValue("Int", int64(11))); err != nil {
				t.Fatal(err)
			}
			originalShared, err := loadFieldValue(module, source, "Shared")
			if err != nil || originalShared.payload().(*vmSlice).valueAt(0).materializedData() != int64(11) {
				t.Fatalf("reference field did not retain its alias: %v", err)
			}
			module.vm.heap.poisonCollected = true
			for _, major := range []bool{false, true} {
				if _, err := module.vm.heap.collect([]guestRef{copied.ref}, major); err != nil {
					t.Fatal(err)
				}
				copiedItems, err := loadFieldValue(module, copied, "Items")
				if err != nil || copiedItems.payload().(*vmArray).valueAt(0).materializedData() != int64(1) {
					t.Fatalf("value field was not copied independently: %v", err)
				}
				copiedShared, err := loadFieldValue(module, copied, "Shared")
				if err != nil || copiedShared.payload().(*vmSlice).valueAt(0).materializedData() != int64(11) {
					t.Fatalf("reference field did not survive collection: %v", err)
				}
			}
		})
	}
}

func TestManagedBooleanStoreReplacesPreviousValue(t *testing.T) {
	var storage [1]byte
	for _, want := range []bool{true, false, true, false} {
		if err := writeScalarBytes(storage[:], 0, newVMValue("Bool", want)); err != nil {
			t.Fatal(err)
		}
		if got := storage[0] != 0; got != want {
			t.Fatalf("boolean storage = %t, want %t", got, want)
		}
	}
}

func TestManagedSliceImportInitializesSpareCapacity(t *testing.T) {
	module := newManagedValueTestModule()
	items := make([]vmValue, 1, 4)
	items[0] = newVMValue("Int32", int64('界'))
	value := module.adoptValue(newOwnedSliceValue("Slice<Int32>", items))
	if err := module.vm.takeAllocationFailure(); err != nil {
		t.Fatal(err)
	}
	slice, ok := materializeManagedSlice(value)
	if !ok || slice.Len != 1 || slice.Cap != 4 {
		t.Fatalf("imported slice = %#v", slice)
	}
	slice.Len = slice.Cap
	for index, want := range []int64{'界', 0, 0, 0} {
		if got, ok := slice.valueAt(index).signedValue(); !ok || got != want {
			t.Fatalf("backing[%d] = %d, want %d", index, got, want)
		}
	}
}

func TestManagedStringImportUsesDescriptorAndByteBacking(t *testing.T) {
	module := newManagedValueTestModule()
	value := module.adoptValue(newVMValue("String", "hello"))
	if !value.ref.valid() || value.heap != module.vm.heap {
		t.Fatalf("string was not published: %+v", value)
	}
	var descriptor [24]byte
	if err := module.vm.heap.read(value.ref, 0, descriptor[:]); err != nil {
		t.Fatal(err)
	}
	backing := guestRef{slot: binary.LittleEndian.Uint32(descriptor[0:4]), generation: binary.LittleEndian.Uint32(descriptor[4:8])}
	if !backing.valid() || binary.LittleEndian.Uint64(descriptor[16:24]) != 5 {
		t.Fatalf("string descriptor = %x", descriptor)
	}
	bytes := make([]byte, 5)
	if err := module.vm.heap.read(backing, 0, bytes); err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "hello" {
		t.Fatalf("string backing = %q", bytes)
	}
}

func TestManagedCellSlotUsesPageAsCanonicalStorage(t *testing.T) {
	module := newManagedValueTestModule()
	slot := &slot{module: module, typ: predeclaredRuntimeTypes["Any"], value: newVMValue("Int", int64(3)), initialized: true}
	cell, err := slot.ensureManagedCell()
	if err != nil {
		t.Fatal(err)
	}
	if slot.value.Type.Valid() || slot.initialized {
		t.Fatal("managed cell retained a host value mirror")
	}
	if err := cell.write(newVMValue("Int", int64(7))); err != nil {
		t.Fatal(err)
	}
	value, initialized := slot.snapshot()
	if !initialized {
		t.Fatal("managed cell snapshot was not initialized")
	}
	inner, ok := value.payload().(vmValue)
	if !ok {
		t.Fatalf("managed cell snapshot payload = %#v, want dynamic value", value.payload())
	}
	if got, ok := inner.signedValue(); !ok || got != 7 {
		t.Fatalf("managed cell snapshot payload = %#v, want signed 7", inner)
	}
}

func TestManagedArrayImportWritesInlineScalarElements(t *testing.T) {
	module := newManagedValueTestModule()
	value := newVMValue("Array<3, Int>", []vmValue{
		newVMValue("Int", int64(3)), newVMValue("Int", int64(5)), newVMValue("Int", int64(7)),
	})
	value = module.adoptValue(value)
	if !value.ref.valid() {
		t.Fatalf("array was not published: %+v", value)
	}
	for index, want := range []int64{3, 5, 7} {
		word, err := module.vm.heap.readWord(value.ref, uint32(index*8))
		if err != nil || int64(word) != want {
			t.Fatalf("array[%d] = %d, err=%v", index, word, err)
		}
	}
	array, ok := value.payload().(*vmArray)
	if !ok || array == nil {
		t.Fatalf("published array view = %#v", value.payload())
	}
	for index, want := range []int64{3, 5, 7} {
		got, ok := array.valueAt(index).signedValue()
		if !ok || got != want {
			t.Fatalf("array view[%d] = %d (%v)", index, got, ok)
		}
	}
	if err := array.setValueAt(1, newVMValue("Int", int64(9))); err != nil {
		t.Fatal(err)
	}
	word, err := module.vm.heap.readWord(value.ref, 8)
	if err != nil || int64(word) != 9 {
		t.Fatalf("array page mutation = %d, err=%v", word, err)
	}
}

func TestManagedAggregateZeroPublishesTypedLanes(t *testing.T) {
	module := newManagedValueTestModule()
	value := module.adoptValue(vmValue{Type: runtimeTypeFromText("Array<3,struct{Count:Int64,Data:Slice<Int>}>")})
	if err := module.vm.takeAllocationFailure(); err != nil {
		t.Fatal(err)
	}
	array, ok := value.payload().(*vmArray)
	if !ok || array.Len != 3 {
		t.Fatalf("zero array view = %#v", value.payload())
	}
	for index := range 3 {
		structure, ok := array.valueAt(index).payload().(*vmStruct)
		if !ok {
			t.Fatalf("zero array element %d has no typed struct", index)
		}
		count, ok := structure.fieldAt(0)
		if got, numeric := count.signedValue(); !ok || !numeric || got != 0 {
			t.Fatalf("zero numeric field = %+v", count)
		}
		data, ok := structure.fieldAt(1)
		if !ok || !isNilRuntimeValue(data) {
			t.Fatalf("zero slice field = %+v", data)
		}
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := value.payload().(*vmArray); !ok {
		t.Fatal("typed zero array lost storage after collection")
	}
}

func TestManagedSliceImportPublishesBackingDescriptor(t *testing.T) {
	module := newManagedValueTestModule()
	value := module.adoptValue(newSliceValue("Slice<Int>", []vmValue{
		newVMValue("Int", int64(11)), newVMValue("Int", int64(13)),
	}))
	if !value.ref.valid() {
		t.Fatalf("slice was not published: %+v", value)
	}
	var descriptor [32]byte
	if err := module.vm.heap.read(value.ref, 0, descriptor[:]); err != nil {
		t.Fatal(err)
	}
	backing := guestRef{slot: binary.LittleEndian.Uint32(descriptor[0:4]), generation: binary.LittleEndian.Uint32(descriptor[4:8])}
	if !backing.valid() || binary.LittleEndian.Uint64(descriptor[16:24]) != 2 || binary.LittleEndian.Uint64(descriptor[24:32]) != 2 {
		t.Fatalf("slice descriptor = %x", descriptor)
	}
	for index, want := range []int64{11, 13} {
		word, err := module.vm.heap.readWord(backing, uint32(index*8))
		if err != nil || int64(word) != want {
			t.Fatalf("slice[%d] = %d, err=%v", index, word, err)
		}
	}
	view, ok := value.payload().(*vmSlice)
	if !ok || view == nil {
		t.Fatalf("published slice view = %#v", value.payload())
	}
	if got, ok := view.valueAt(1).signedValue(); !ok || got != 13 {
		t.Fatalf("slice view element = %d (%v)", got, ok)
	}
	if err := view.setValueAt(1, newVMValue("Int", int64(17))); err != nil {
		t.Fatal(err)
	}
	word, err := module.vm.heap.readWord(backing, 8)
	if err != nil || int64(word) != 17 {
		t.Fatalf("slice page mutation = %d, err=%v", word, err)
	}
}

func TestManagedSliceViewPublishesZeroBasedBackingOffset(t *testing.T) {
	module := newManagedValueTestModule()
	base := newSliceValue("Slice<Int>", []vmValue{
		newVMValue("Int", int64(3)), newVMValue("Int", int64(5)), newVMValue("Int", int64(7)),
	})
	view := newSliceViewValue(base.Type, base.Data.(*vmSlice), 1, 2, 2)
	value := module.adoptValue(view)
	if !value.ref.valid() {
		t.Fatalf("slice view was not published: %+v", value)
	}
	var descriptor [32]byte
	if err := module.vm.heap.read(value.ref, 0, descriptor[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(descriptor[8:16]); got != 0 {
		t.Fatalf("detached view backing offset = %d, want zero", got)
	}
	managed, ok := value.payload().(*vmSlice)
	if !ok || managed == nil {
		t.Fatalf("published view = %#v", value.payload())
	}
	for index, want := range []int64{5, 7} {
		got, ok := managed.valueAt(index).signedValue()
		if !ok || got != want {
			t.Fatalf("view[%d] = %d (%v), want %d", index, got, ok, want)
		}
	}
}

func TestManagedArrayViewImportPreservesBackingOffset(t *testing.T) {
	module := newManagedValueTestModule()
	base := module.adoptValue(newSliceValue("Slice<Int>", []vmValue{
		newVMValue("Int", int64(3)), newVMValue("Int", int64(5)), newVMValue("Int", int64(7)),
	}))
	baseSlice, ok := base.payload().(*vmSlice)
	if !ok || baseSlice == nil {
		t.Fatalf("base slice = %#v", base.payload())
	}
	viewValue := module.adoptValue(newSliceViewValue(base.Type, baseSlice, 1, 2, 2))
	view, ok := viewValue.payload().(*vmSlice)
	if !ok || view == nil || view.managedBase == 0 {
		t.Fatalf("managed view = %#v", viewValue.payload())
	}
	array := &vmArray{vmSlice: *view}
	array.Len, array.Cap = 2, 2
	imported, err := module.importDetachedValue(newVMValue("Array<2, Int>", array), map[uintptr]guestRef{})
	if err != nil {
		t.Fatal(err)
	}
	if imported.ref != view.managedRef || imported.offset != view.managedBase {
		t.Fatalf("array view location = ref %+v offset %d, want ref %+v offset %d", imported.ref, imported.offset, view.managedRef, view.managedBase)
	}
	managed, ok := materializeManagedArray(imported)
	if !ok || managed == nil {
		t.Fatalf("materialized array = %#v", managed)
	}
	if err := managed.setValueAt(0, newVMValue("Int", int64(11))); err != nil {
		t.Fatal(err)
	}
	if got, ok := baseSlice.valueAt(1).signedValue(); !ok || got != 11 {
		t.Fatalf("backing value = %d (%v), want 11", got, ok)
	}
}

func TestManagedEmptySliceMaterializesWithoutBackingTicket(t *testing.T) {
	module := newManagedValueTestModule()
	value := module.adoptValue(newSliceValue("Slice<Int>", nil))
	slice, ok := value.payload().(*vmSlice)
	if !ok || slice == nil || slice.Len != 0 || slice.Cap != 0 {
		t.Fatalf("empty managed slice = %#v, %v", value.payload(), ok)
	}
	if got := slice.values(); got == nil || len(got) != 0 {
		t.Fatalf("empty managed slice values = %#v", got)
	}
}

func TestManagedAndFrameSliceDescriptorsPreserveNil(t *testing.T) {
	module := newManagedValueTestModule()
	typ := coerceRuntimeType("Slice<Int>")
	layout, err := module.vm.valueLayouts.layoutFor(typ)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []vmValue{newVMValue(typ, nil), newSliceValue(typ, []vmValue{})} {
		wantNil := isNilRuntimeValue(value)
		cell, err := module.newManagedCell(typ, value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := cell.read()
		if err != nil || isNilRuntimeValue(got) != wantNil {
			t.Fatalf("managed slice nil=%v, want=%v, err=%v", isNilRuntimeValue(got), wantNil, err)
		}
		var raw [32]byte
		if err := cell.heap.read(cell.ref, 0, raw[:]); err != nil {
			t.Fatal(err)
		}
		got = readInlineAggregateOwned(module, typ, layout, raw[:], nil)
		if isNilRuntimeValue(got) != wantNil {
			t.Fatalf("frame slice nil=%v, want=%v", isNilRuntimeValue(got), wantNil)
		}
	}
}

func TestManagedStructImportReadsAndWritesInlineFields(t *testing.T) {
	module := newManagedValueTestModule()
	table := &types.TypeTable{}
	ref, err := types.NewParser("managed/test", table).Parse("struct{Count:Int,Flag:Bool}")
	if err != nil {
		t.Fatal(err)
	}
	typ := runtimeTypeWithTable(ref, table)
	value := newRuntimeStructValue(module, typ, map[string]vmValue{
		"Count": newVMValue("Int", int64(23)), "Flag": newBoolValue(true),
	})
	value, err = module.importDetachedValue(value, map[uintptr]guestRef{})
	if err != nil {
		t.Fatalf("struct import: %v", err)
	}
	if !value.ref.valid() {
		t.Fatalf("struct was not published: %+v", value)
	}
	structure, ok := value.payload().(*vmStruct)
	if !ok || structure == nil {
		t.Fatalf("published struct view = %#v", value.payload())
	}
	count, ok := structure.fieldAt(0)
	if !ok || count.materializedData() != int64(23) {
		t.Fatalf("struct count = %#v, %v", count, ok)
	}
	flag, ok := structure.fieldAt(1)
	if !ok || flag.materializedData() != true {
		t.Fatalf("struct flag = %#v, %v", flag, ok)
	}
	setRuntimeStructField(structure, "Count", newVMValue("Int", int64(31)))
	count, ok = structure.fieldAt(0)
	if !ok || count.materializedData() != int64(31) {
		t.Fatalf("updated struct count = %#v, %v", count, ok)
	}
}

func TestManagedResourceImportUsesStableHandle(t *testing.T) {
	module := newManagedValueTestModule()
	resource := newVMMap(0)
	value := module.adoptValue(newVMValue("Map<Int,Int>", resource))
	if !value.ref.valid() {
		t.Fatalf("map was not published: %+v", value)
	}
	if got, ok := value.payload().(*vmMap); !ok || got == nil || got == resource {
		t.Fatalf("map handle payload = %#v, want an owned managed handle", value.payload())
	}
}

func TestManagedMapStoresEntriesInTypedPagesAndTracesChildren(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := newMapValue(module, "Map<String, String>", []vmValue{
		newVMValue("String", "left"), newVMValue("String", "right"),
	})
	if err != nil {
		t.Fatal(err)
	}
	managed, ok := value.payload().(*vmMap)
	if !ok || managed == nil || managed.managed == nil {
		t.Fatalf("map payload = %#v", value.payload())
	}
	if len(managed.managed.heap.pages) == 0 {
		t.Fatal("map did not allocate managed pages")
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	entries := managed.snapshot()
	if len(entries) != 1 {
		t.Fatalf("map entries after collection = %d", len(entries))
	}
	for _, entry := range entries {
		if entry.Key.materializedData() != "left" || entry.Value.materializedData() != "right" {
			t.Fatalf("map entry after collection = %#v", entry)
		}
	}
}

func TestManagedWaitableRetainsBufferedStringAcrossCollection(t *testing.T) {
	module := newManagedValueTestModule()
	channel, err := makeWaitableValue(module, "Waitable<String>", newVMValue("Int", int64(2)))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := waitableTrySendValue(module, channel, newVMValue("String", "payload")); err != nil || !ok {
		t.Fatalf("send = %v, %v", ok, err)
	}
	if _, err := module.vm.heap.collect([]guestRef{channel.ref}, true); err != nil {
		t.Fatal(err)
	}
	value, ok, _, err := waitableTryRecvValue(module, channel)
	if err != nil || !ok || value.materializedData() != "payload" {
		t.Fatalf("receive = %#v, %v, %v", value, ok, err)
	}
}

func TestManagedWaitableIdentityCarriesFullPreparedTypeID(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := makeWaitableValue(module, "Waitable<String>", newVMValue("Int", int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := module.vm.valueLayouts.layoutFor(value.Type)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := module.vm.heap.resolveObject(value.ref)
	if err != nil {
		t.Fatal(err)
	}
	if got := module.vm.heap.layouts[entry.layout].typeID; got != want.ID {
		t.Fatalf("managed waitable type id = %d, want %d", got, want.ID)
	}
}

func TestManagedPointerTargetsStableCellAcrossCollection(t *testing.T) {
	module := newManagedValueTestModule()
	cell := newSlot(coerceRuntimeType("Int"), module, false)
	cell.publish(newVMValue("Int", int64(7)))
	pointer := newSlotPointerValue("Int", "cell", cell)
	if !pointer.ref.valid() {
		t.Fatalf("pointer was not published: %+v", pointer)
	}
	if _, err := module.vm.heap.collect([]guestRef{pointer.ref}, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := pointerValue(pointer)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolved.loadValue()
	if err != nil || got.materializedData() != int64(7) {
		t.Fatalf("pointer load = %#v, %v", got, err)
	}
	if err := resolved.storeValue(newVMValue("Int", int64(11)), true); err != nil {
		t.Fatal(err)
	}
	if got := cell.load().materializedData(); got != int64(11) {
		t.Fatalf("pointer store = %#v", got)
	}
}

func TestManagedPointerCopySurvivesMapRemovalAndCollection(t *testing.T) {
	module := newManagedValueTestModule()
	cell := newSlot(coerceRuntimeType("Int"), module, false)
	cell.publish(newVMValue("Int", int64(7)))
	pointer := newSlotPointerValue("Int", "target", cell)
	container, err := newMapValueWithCapacityBatch(module, "Map<Int, Ptr<Int>>", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := newVMValue("Int", int64(1))
	if _, err := setIndexValue(module, container, key, pointer); err != nil {
		t.Fatal(err)
	}
	managed, _ := materializeManagedMap(container)
	entry, ok := managed.managed.loadEntry(vmMapKey{Kind: vmMapKeyInt, TypeIdentity: "Int", Int64: 1})
	if !ok {
		t.Fatal("pointer entry is missing")
	}
	saved := entry.Value
	if err := deleteValue(module, container, key); err != nil {
		t.Fatal(err)
	}
	if _, err := module.vm.heap.collect([]guestRef{saved.ref}, true); err != nil {
		t.Fatal(err)
	}
	resolved, err := pointerValue(saved)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolved.loadValue()
	if err != nil || got.materializedData() != int64(7) {
		t.Fatalf("copied pointer after entry reclamation = %#v, %v", got, err)
	}
	if err := resolved.storeValue(newVMValue("Int", int64(11)), true); err != nil {
		t.Fatal(err)
	}
	if got := cell.load().materializedData(); got != int64(11) {
		t.Fatalf("pointer target = %#v", got)
	}
}

func TestManagedNumericStoragePreservesFloatingBitPatterns(t *testing.T) {
	values := []vmValue{
		newVMValue("Float32", math.Float32frombits(0x80000000)),
		newVMValue("Float32", math.Float32frombits(1)),
		newVMValue("Float32", float32(math.Inf(1))),
		newVMValue("Float64", math.Float64frombits(1)),
		newVMValue("Complex64", complex(float32(math.Inf(1)), float32(math.Inf(-1)))),
		newVMValue("Complex128", complex(math.Float64frombits(1), math.Copysign(0, -1))),
	}
	for index, value := range values {
		t.Run(fmt.Sprintf("%s/%d", value.Type, index), func(t *testing.T) {
			module := newManagedValueTestModule()
			layout, err := module.vm.valueLayouts.layoutFor(value.Type)
			if err != nil {
				t.Fatal(err)
			}
			var expected [16]byte
			if number, ok := value.floatValue(); ok {
				if value.Type.Primitive(types.PrimitiveFloat32) {
					binary.LittleEndian.PutUint32(expected[:], math.Float32bits(float32(number)))
				} else {
					binary.LittleEndian.PutUint64(expected[:], math.Float64bits(number))
				}
			} else {
				number := value.Data.(complex128)
				if value.Type.Primitive(types.PrimitiveComplex64) {
					binary.LittleEndian.PutUint32(expected[:], math.Float32bits(float32(real(number))))
					binary.LittleEndian.PutUint32(expected[4:], math.Float32bits(float32(imag(number))))
				} else {
					binary.LittleEndian.PutUint64(expected[:], math.Float64bits(real(number)))
					binary.LittleEndian.PutUint64(expected[8:], math.Float64bits(imag(number)))
				}
			}
			cell, err := module.newManagedCell(value.Type, value)
			if err != nil {
				t.Fatal(err)
			}
			stored := vmValue{Type: value.Type, ref: cell.ref, heap: cell.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}
			for _, source := range []vmValue{value, stored} {
				if err := cell.write(source); err != nil {
					t.Fatal(err)
				}
				var raw [16]byte
				if err := cell.heap.read(cell.ref, 0, raw[:layout.Stride]); err != nil {
					t.Fatal(err)
				}
				if raw != expected {
					t.Fatalf("physical scalar = %x, want %x", raw, expected)
				}
				got, err := cell.read()
				if err != nil {
					t.Fatal(err)
				}
				clear(raw[:])
				if err := module.writeInlineValue(raw[:layout.Stride], got, map[uintptr]guestRef{}); err != nil {
					t.Fatal(err)
				}
				if raw != expected {
					t.Fatalf("decoded scalar = %x, want %x", raw, expected)
				}
			}
		})
	}
}

func TestManagedFunctionUsesNumericOwnerTicket(t *testing.T) {
	module := newManagedValueTestModule()
	original := functionRef{ModulePath: "main", FunctionID: "fn.entry", exact: module}
	value := module.adoptValue(newVMValue("Function", original))
	if !value.ref.valid() {
		t.Fatalf("function was not published: %+v", value)
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	resolved, ok := value.payload().(functionRef)
	if !ok || resolved.FunctionID != original.FunctionID || resolved.exact != original.exact {
		t.Fatalf("function payload = %#v", value.payload())
	}
}

func TestManagedFunctionCaptureIsTracedFromFunctionRecord(t *testing.T) {
	module := newManagedValueTestModule()
	captured := newSlot(coerceRuntimeType("String"), module, false)
	captured.publish(newVMValue("String", "captured"))
	value := module.adoptValue(newVMValue("Function", functionRef{
		ModulePath: "main", FunctionID: "fn.capture", exact: module,
		captureSlots: []*slot{captured},
	}))
	if !value.ref.valid() {
		t.Fatalf("closure was not published: %+v", value)
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	payload, _ := materializeManagedFunction(value)
	ref, ok := payload.(functionRef)
	if !ok || len(ref.captureSlots) != 1 || ref.captureSlots[0] == nil {
		t.Fatalf("materialized closure captures = %#v", ref.captureSlots)
	}
	if got := ref.captureSlots[0].load().materializedData(); got != "captured" {
		t.Fatalf("captured value after collection = %#v", got)
	}
}

func TestManagedDynamicImportRetainsConcreteReference(t *testing.T) {
	module := newManagedValueTestModule()
	value := module.adoptValue(newVMValue("Any", newVMValue("String", "hello")))
	inner, ok := value.payload().(vmValue)
	if !ok || inner.payload() != "hello" {
		t.Fatalf("dynamic payload = %#v, %v", value.payload(), ok)
	}
}

func TestManagedSliceArrayPointerUsesPhysicalElementIdentity(t *testing.T) {
	module := newManagedValueTestModule()
	source := module.adoptValue(newSliceValue("Slice<Int>", []vmValue{
		newVMValue("Int", int64(1)), newVMValue("Int", int64(2)), newVMValue("Int", int64(3)),
	}))
	view, ok := source.payload().(*vmSlice)
	if !ok || view == nil {
		t.Fatalf("managed slice payload = %#v", source.payload())
	}
	target := runtimeTypeFromText("Ptr<Array<2, Int>>")
	first, handled, err := module.convertSliceToArrayPointer(source, target)
	if err != nil || !handled {
		t.Fatalf("first conversion: handled=%v err=%v", handled, err)
	}
	second, handled, err := module.convertSliceToArrayPointer(newSliceViewValue(source.Type, view, 1, 2, 2), target)
	if err != nil || !handled {
		t.Fatalf("second conversion: handled=%v err=%v", handled, err)
	}
	if equal, err := module.equalValues(first, second); err != nil || equal {
		t.Fatalf("different managed slices compare equal: %v, %v", equal, err)
	}
	third, _, err := module.convertSliceToArrayPointer(newSliceViewValue(source.Type, view, 1, 2, 2), target)
	if err != nil {
		t.Fatal(err)
	}
	if equal, err := module.equalValues(second, third); err != nil || !equal {
		t.Fatalf("same managed slice identity is unstable: %v, %v", equal, err)
	}
}
