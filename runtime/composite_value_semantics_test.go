package runtime

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestNestedStructAssignmentCopiesOnMutation(t *testing.T) {
	module := &moduleInstance{}
	intType := runtimeTypeFromText("Int")
	childType := runtimeTypeFromText("struct{X:Int}")
	parentType := runtimeTypeFromText("struct{Child:struct{X:Int}}")
	child, err := newStructValue(module, childType, []string{"X"}, []vmValue{newVMValue(intType, int64(1))})
	if err != nil {
		t.Fatal(err)
	}
	value, err := newStructValue(module, parentType, []string{"Child"}, []vmValue{child})
	if err != nil {
		t.Fatal(err)
	}

	left := module.cloneValueForStore(value)
	right := module.cloneValueForStore(left)
	cell := newSlot(parentType, module, false)
	if err := cell.store(right); err != nil {
		t.Fatal(err)
	}
	pointer := newPathPointerValue(intType, "parent.Child.X", cell, []ir.AddressPathSegment{
		{Kind: "field", Field: "Child"},
		{Kind: "field", Field: "X"},
	}, nil)
	if err := storePointer(pointer, newVMValue(intType, int64(2))); err != nil {
		t.Fatalf("mutate copied struct: %v", err)
	}
	updated := cell.load()

	leftChild, err := loadFieldValue(module, left, "Child")
	if err != nil {
		t.Fatalf("load original child: %v", err)
	}
	leftX, err := loadFieldValue(module, leftChild, "X")
	if err != nil {
		t.Fatalf("load original field: %v", err)
	}
	rightChild, err := loadFieldValue(module, updated, "Child")
	if err != nil {
		t.Fatalf("load copied child: %v", err)
	}
	rightX, err := loadFieldValue(module, rightChild, "X")
	if err != nil {
		t.Fatalf("load copied field: %v", err)
	}
	if leftX.materializedData() != int64(1) || rightX.materializedData() != int64(2) {
		t.Fatalf("nested assignment changed both values: left=%v right=%v", leftX.Data, rightX.Data)
	}
}

func TestZeroStructArrayFieldSliceMutatesField(t *testing.T) {
	module := &moduleInstance{}
	containerType := runtimeTypeFromText("struct{Block:Array<4, Uint8>}")
	value := module.zeroValue(containerType)
	cell := newSlot(containerType, module, false)
	if err := cell.store(value); err != nil {
		t.Fatal(err)
	}
	arrayType := runtimeTypeFromText("Array<4, Uint8>")
	arrayPointer := newPathPointerValue(arrayType, "container.Block", cell, []ir.AddressPathSegment{{Kind: "field", Field: "Block"}}, nil)
	view, err := sliceValue(
		module,
		arrayPointer,
		newVMValue("Int", int64(0)),
		newVMValue("Int", int64(4)),
		vmValue{},
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []uint64{1, 2, 3, 4} {
		if _, err := setIndexValue(module, view, newVMValue("Int", int64(index)), newVMValue("Uint8", want)); err != nil {
			t.Fatalf("set view index %d: %v", index, err)
		}
	}

	value = cell.load()
	array, err := loadFieldValue(module, value, "Block")
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []uint64{1, 2, 3, 4} {
		got, err := indexValue(module, array, newVMValue("Int", int64(index)))
		if err != nil {
			t.Fatalf("load field index %d: %v", index, err)
		}
		if got.materializedData() != want {
			t.Fatalf("field index %d = %v, want %d", index, got.Data, want)
		}
	}

	copyValue := module.cloneValueForStore(value)
	copyArray, err := loadFieldValue(module, copyValue, "Block")
	if err != nil {
		t.Fatal(err)
	}
	copyView, err := sliceValue(
		module,
		copyArray,
		newVMValue("Int", int64(0)),
		newVMValue("Int", int64(4)),
		vmValue{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, copyView, newVMValue("Int", int64(0)), newVMValue("Uint8", uint64(9))); err != nil {
		t.Fatal(err)
	}
	original, err := indexValue(module, array, newVMValue("Int", int64(0)))
	if err != nil {
		t.Fatal(err)
	}
	if original.materializedData() != uint64(1) {
		t.Fatalf("mutating copied field changed original to %v", original.Data)
	}
}

func TestAppendInitializesSpareSliceCapacityWithTypedZeros(t *testing.T) {
	module := &moduleInstance{}
	value := newSliceValue("Slice<Slice<Int>>", nil)
	value, err := appendValue(module, value, []vmValue{module.zeroValue("Slice<Int>")}, false)
	if err != nil {
		t.Fatal(err)
	}
	header := value.Data.(*vmSlice)
	if header.Cap <= header.Len {
		t.Skip("append growth did not reserve spare capacity")
	}
	extended, err := sliceValue(module, value, newVMValue("Int", int64(0)), newVMValue("Int", int64(header.Cap)), vmValue{})
	if err != nil {
		t.Fatal(err)
	}
	zero, err := indexValue(module, extended, newVMValue("Int", int64(header.Len)))
	if err != nil {
		t.Fatal(err)
	}
	if zero.Type.String() != "Slice<Int>" || zero.Data != nil {
		t.Fatalf("spare element = (%s, %#v), want typed nil Slice<Int>", zero.Type, zero.Data)
	}
}
