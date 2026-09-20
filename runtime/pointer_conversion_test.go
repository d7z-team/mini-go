package runtime

import (
	"fmt"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func FuzzPointerConversionViews(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{2, 1, 0, 2})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 256 {
			t.Skip()
		}
		testPointerConversionViews(t, operations)
	})
}

func TestPointerConversionViewsShareOriginalStorage(t *testing.T) {
	operations := make([]byte, 3000)
	for i := range operations {
		operations[i] = byte(i % 3)
	}
	testPointerConversionViews(t, operations)
}

func TestArrayElementAddressesDistinguishScalarIndices(t *testing.T) {
	module := &moduleInstance{}
	root := newSlot(coerceRuntimeType("Array<2, Int>"), module, false)
	if err := root.store(newVMValue(root.typ, []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2))})); err != nil {
		t.Fatal(err)
	}
	index := newSlot(coerceRuntimeType("Int"), module, false)
	frame := &frame{function: loadedFunction{LocalIndexes: map[string]int{"index": 0}}, localCells: []*slot{index}}
	payload := ir.AddressPayload{Path: []ir.AddressPathSegment{{Kind: "index", Local: "index"}}}
	var pointers []vmValue
	for _, value := range []int64{0, 1, 0} {
		if err := index.store(newVMValue("Int", value)); err != nil {
			t.Fatal(err)
		}
		pointer, err := frame.pathAddress(root, payload)
		if err != nil {
			t.Fatal(err)
		}
		pointers = append(pointers, pointer)
	}
	if equal, err := module.equalValues(pointers[0], pointers[1]); err != nil || equal {
		t.Fatalf("different array elements compare equal: %t, %v", equal, err)
	}
	if equal, err := module.equalValues(pointers[0], pointers[2]); err != nil || !equal {
		t.Fatalf("same array element has unstable identity: %t, %v", equal, err)
	}
}

func TestArrayAssignmentPreservesElementViews(t *testing.T) {
	module := &moduleInstance{}
	root := newSlot(coerceRuntimeType("Array<2, Int>"), module, false)
	arrayValue := func(a, b int64) vmValue {
		return newVMValue(root.typ, []vmValue{newVMValue("Int", a), newVMValue("Int", b)})
	}
	if err := root.store(arrayValue(1, 2)); err != nil {
		t.Fatal(err)
	}
	array := root.load().Data.(*vmArray)
	view := newSliceViewValue("Slice<Int>", &array.vmSlice, 0, 2, 2)
	if err := root.store(arrayValue(3, 4)); err != nil {
		t.Fatal(err)
	}
	if got, _ := asInt64(view.Data.(*vmSlice).valueAt(0)); got != 3 {
		t.Fatalf("array assignment detached existing slice: %d", got)
	}
	if _, err := setIndexValue(module, view, newVMValue("Int", int64(1)), newVMValue("Int", int64(9))); err != nil {
		t.Fatal(err)
	}
	if got, _ := asInt64(root.load().Data.(*vmArray).valueAt(1)); got != 9 {
		t.Fatalf("slice mutation detached from assigned array: %d", got)
	}
}

func TestSequenceElementPointersKeepBackingAndShareIdentity(t *testing.T) {
	module := &moduleInstance{}
	array := newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2))})
	root := &slot{typ: array.Type, module: module, value: array, initialized: true}
	view := newSliceViewValue("Slice<Int>", &array.Data.(*vmArray).vmSlice, 0, 2, 2)
	sliceRoot := &slot{typ: view.Type, module: module, value: view, initialized: true}
	index := &slot{typ: coerceRuntimeType("Int"), module: module, value: newVMValue("Int", int64(1)), initialized: true}
	frame := &frame{function: loadedFunction{LocalIndexes: map[string]int{"index": 0}}, localCells: []*slot{index}}
	payload := ir.AddressPayload{Path: []ir.AddressPathSegment{{Kind: "index", Local: "index"}}}
	var pointers []vmValue
	for _, cell := range []*slot{root, sliceRoot} {
		ordinary, err := frame.pathAddress(cell, payload)
		if err != nil {
			t.Fatal(err)
		}
		reflected, err := reflectIndexPointer(module, newSlotPointerValue(cell.typ, fmt.Sprintf("slot:%p", cell), cell), 1)
		if err != nil {
			t.Fatal(err)
		}
		pointers = append(pointers, ordinary, reflected)
	}
	if err := sliceRoot.store(newOwnedSliceValue(view.Type, []vmValue{newVMValue("Int", int64(8)), newVMValue("Int", int64(9))})); err != nil {
		t.Fatal(err)
	}
	for _, pointer := range pointers {
		if equal, err := module.equalValues(pointers[0], pointer); err != nil || !equal {
			t.Fatalf("element identities disagree: %v, %v", equal, err)
		}
		if err := storePointer(pointer, newVMValue("Int", int64(7))); err != nil {
			t.Fatal(err)
		}
		value, err := derefPointer(pointer)
		if got, _ := asInt64(value); err != nil || got != 7 {
			t.Fatalf("element load: %d, %v", got, err)
		}
	}
	if got, _ := asInt64(array.Data.(*vmArray).valueAt(1)); got != 7 {
		t.Fatalf("pointer did not write original backing: %d", got)
	}
	if got, _ := asInt64(sliceRoot.load().Data.(*vmSlice).valueAt(1)); got != 9 {
		t.Fatalf("pointer followed replacement slice: %d", got)
	}
}

func TestStructArrayCopiesRemainIndependentAcrossRepeatedAssignments(t *testing.T) {
	module := &moduleInstance{}
	original := newRuntimeStructValue(nil, "struct{Values:Array<2, Int>}", map[string]vmValue{
		"Values": newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2))}),
	})
	first := module.cloneValueForStore(original)
	second := module.cloneValueForStore(original)
	array, ok := structValueField(second.Data, "Values")
	if !ok {
		t.Fatal("missing array field")
	}
	if _, err := setIndexValue(module, array, newVMValue("Int", int64(0)), newVMValue("Int", int64(9))); err != nil {
		t.Fatal(err)
	}
	for _, value := range []vmValue{original, first} {
		array, _ := structValueField(value.Data, "Values")
		if got, _ := asInt64(array.Data.(*vmArray).valueAt(0)); got != 1 {
			t.Fatalf("struct copy shared array storage: %d", got)
		}
	}
}

func TestZeroStructArrayElementAddressMaterializesOwningField(t *testing.T) {
	module := &moduleInstance{}
	typ := coerceRuntimeType("struct{Values:Array<2, Int>}")
	root := newSlot(typ, module, false)
	index := &slot{typ: coerceRuntimeType("Int"), module: module, value: newVMValue("Int", int64(0)), initialized: true}
	frame := &frame{function: loadedFunction{LocalIndexes: map[string]int{"index": 0}}, localCells: []*slot{index}}
	payload := ir.AddressPayload{Path: []ir.AddressPathSegment{{Kind: "field", Field: "Values"}, {Kind: "index", Local: "index"}}}
	pointer, err := frame.pathAddress(root, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := storePointer(pointer, newVMValue("Int", int64(7))); err != nil {
		t.Fatal(err)
	}
	array, err := loadFieldValue(module, root.load(), "Values")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := asInt64(array.Data.(*vmArray).valueAt(0)); got != 7 {
		t.Fatalf("pointer wrote a detached zero array: %d", got)
	}
	copied := module.cloneValueForStore(root.load())
	if err := storePointer(pointer, newVMValue("Int", int64(8))); err != nil {
		t.Fatal(err)
	}
	array, _ = loadFieldValue(module, copied, "Values")
	if got, _ := asInt64(array.Data.(*vmArray).valueAt(0)); got != 7 {
		t.Fatalf("materialized zero field shared backing after copy: %d", got)
	}
}

func TestReflectedZeroArrayElementAddressMaterializesOwningField(t *testing.T) {
	module := &moduleInstance{}
	typ := coerceRuntimeType("struct{Values:Array<2, Int>}")
	root := newSlot(typ, module, false)
	parent := newSlotPointerValue(typ, "root", root)
	field := reflectStructFieldPointer(module, parent, TypeFieldInfo{Name: "Values", RuntimeType: coerceRuntimeType("Array<2, Int>")})
	pointer, err := reflectIndexPointer(module, field, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := storePointer(pointer, newVMValue("Int", int64(7))); err != nil {
		t.Fatal(err)
	}
	array, err := loadFieldValue(module, root.load(), "Values")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := asInt64(array.Data.(*vmArray).valueAt(0)); got != 7 {
		t.Fatalf("reflected pointer wrote a detached zero array: %d", got)
	}
}

func testPointerConversionViews(t *testing.T, operations []byte) {
	t.Helper()
	table := types.NewTable()
	decls := make(map[string]types.TypeNode)
	for _, name := range []string{"A", "B", "C"} {
		node := types.TypeNode{ID: types.TypeID(name), Kind: types.Named, Identity: types.TypeKey{ModulePath: "example", DeclID: types.DeclID(name)}, Underlying: types.Builtin(types.PrimitiveInt)}
		if err := table.Add(node); err != nil {
			t.Fatal(err)
		}
		decls[name] = node
	}
	registry := newModuleRegistry()
	if err := registry.addExecutable(&executable{Artifact: ir.Artifact{Module: ir.Module{Path: "example"}, TypeTable: *table}, Types: decls}); err != nil {
		t.Fatal(err)
	}
	module, _ := registry.module("example")
	stored := newVMValue("example.A", int64(1))
	root := reflectCellPointer(module, "example.A", "stored", stored)
	p := root
	for i, operation := range operations {
		var err error
		p, err = module.convertValue(p, []string{"Ptr<example.B>", "Ptr<example.C>", "Ptr<example.A>"}[operation%3])
		if err != nil {
			t.Fatal(err)
		}
		pointer := p.Data.(*vmPointer)
		if pointer != root.Data && pointer.original != root.Data {
			t.Fatal("pointer view retained an intermediate conversion")
		}
		if err := storePointer(p, newVMValue(pointer.Type, int64(i))); err != nil {
			t.Fatal(err)
		}
		loaded, err := derefPointer(p)
		stored = root.Data.(*vmPointer).cell.load()
		if err != nil || loaded.materializedData() != int64(i) || stored.Type.String() != "example.A" || stored.materializedData() != int64(i) {
			t.Fatalf("load=%v storage=%v err=%v", loaded, stored, err)
		}
	}
}

func FuzzSliceArrayPointerStorage(f *testing.F) {
	f.Add([]byte{1, 2, 3}, uint8(1), uint8(2), false)
	f.Add([]byte{}, uint8(0), uint8(0), true)
	f.Add([]byte{7}, uint8(0), uint8(2), true)
	f.Fuzz(func(t *testing.T, input []byte, offset, length uint8, bytes bool) {
		if len(input) > 256 {
			t.Skip()
		}
		module := &moduleInstance{}
		elem := "Int"
		values := make([]vmValue, len(input))
		for i, v := range input {
			values[i] = newVMValue(elem, int64(v))
		}
		original := newOwnedSliceValue("Slice<Int>", values)
		if bytes {
			elem = "Uint8"
			original = newByteSliceValue("Slice<Uint8>", string(input))
		}
		header := original.Data.(*vmSlice)
		start := int(offset) % (len(input) + 1)
		view := newSliceViewValue(original.Type, header, start, len(input)-start, len(input)-start)
		target := fmt.Sprintf("Ptr<Array<%d, %s>>", length, elem)
		p, err := module.convertValue(view, target)
		if int(length) > len(input)-start {
			if err == nil {
				t.Fatal("short slice conversion succeeded")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		other := newSliceViewValue(original.Type, header, start, len(input)-start, len(input)-start)
		q, err := module.convertValue(other, target)
		if err != nil {
			t.Fatal(err)
		}
		if equal, err := module.equalValues(p, q); err != nil || !equal {
			t.Fatalf("same address: %v %v", equal, err)
		}
		loaded, err := derefPointer(p)
		if err != nil {
			t.Fatal(err)
		}
		items := loaded.Data.(*vmArray).values()
		if len(items) != int(length) {
			t.Fatal("array length mismatch")
		}
		if length > 0 {
			if bytes {
				items[0] = newVMValue(elem, uint64(99))
			} else {
				items[0] = newVMValue(elem, int64(99))
			}
			if err := storePointer(p, newVMValue(loaded.Type, items)); err != nil {
				t.Fatal(err)
			}
			got, err := numericAsInt64(header.valueAt(start))
			if err != nil || got != 99 {
				t.Fatalf("shared write: %d %v", got, err)
			}
		}
	})
}

func TestNilSliceToZeroArrayPointer(t *testing.T) {
	module := &moduleInstance{}
	for _, elem := range []string{"Int", "Uint8"} {
		target := "Ptr<Array<0, " + elem + ">>"
		p, err := module.convertValue(newVMValue("Slice<"+elem+">", (*vmSlice)(nil)), target)
		if err != nil || p.Data.(*vmPointer) != nil {
			t.Fatalf("nil conversion: %#v %v", p, err)
		}
		if equal, err := module.equalValues(p, newVMValue(target, nil)); err != nil || !equal {
			t.Fatalf("nil pointer equality: %v %v", equal, err)
		}
		value := newOwnedSliceValue("Slice<Int>", []vmValue{})
		if elem == "Uint8" {
			value = newByteSliceValue("Slice<Uint8>", "")
		}
		p, err = module.convertValue(value, target)
		if err != nil || p.Data.(*vmPointer) == nil {
			t.Fatalf("empty conversion: %#v %v", p, err)
		}
	}
}

func TestOverlappingArrayPointerAssignmentCopiesBeforePublishing(t *testing.T) {
	module := &moduleInstance{}
	for _, bytes := range []bool{false, true} {
		backing := newOwnedSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2)), newVMValue("Int", int64(3)), newVMValue("Int", int64(4))})
		elem := "Int"
		if bytes {
			backing = newByteSliceValue("Slice<Uint8>", "\x01\x02\x03\x04")
			elem = "Uint8"
		}
		header := backing.Data.(*vmSlice)
		left, err := module.convertValue(backing, "Ptr<Array<3, "+elem+">>")
		if err != nil {
			t.Fatal(err)
		}
		right, err := module.convertValue(newSliceViewValue(backing.Type, header, 1, 3, 3), "Ptr<Array<3, "+elem+">>")
		if err != nil {
			t.Fatal(err)
		}
		value, err := derefPointer(left)
		if err != nil {
			t.Fatal(err)
		}
		if err := storePointer(right, value); err != nil {
			t.Fatal(err)
		}
		for index, want := range []int64{1, 1, 2, 3} {
			got, err := numericAsInt64(header.valueAt(index))
			if err != nil || got != want {
				t.Fatalf("byte backing=%t, index=%d: got %d, want %d (%v)", bytes, index, got, want, err)
			}
		}
	}
}
