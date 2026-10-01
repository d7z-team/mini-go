package runtime

import (
	"sync"
	"testing"
)

func TestByteSliceCopyPreservesOverlappingViews(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, direction := range []string{"left", "right", "string", "distinct"} {
			module := &moduleInstance{}
			parent := newByteSliceHeaderValue("Slice<Uint8>", []byte("abcdef"), 6, 6)
			if !compact {
				values := make([]vmValue, 6)
				for i, value := range []byte("abcdef") {
					values[i] = newVMValue("Uint8", uint64(value))
				}
				parent = newSliceValue("Slice<Uint8>", values)
			}
			header := parent.Data.(*vmSlice)
			source := newSliceViewValue("Slice<Uint8>", header, 0, 4, 6)
			destination := newSliceViewValue("Slice<Uint8>", header, 1, 4, 5)
			want := "aabcdf"
			switch direction {
			case "left":
				source, destination = destination, source
				want = "bcdeef"
			case "string":
				source = newVMValue("String", "wxyz!")
				want = "awxyzf"
			case "distinct":
				source = newByteSliceHeaderValue("Slice<Uint8>", []byte("wxyz!"), 5, 5)
				want = "awxyzf"
			}
			copied, err := copyValue(module, destination, source)
			if err != nil || copied.scalar != 4 {
				t.Fatalf("compact=%t %s: copy=%v, %v", compact, direction, copied, err)
			}
			text, _, err := module.convertByteSliceToString(parent)
			if err != nil || text.Data != want {
				t.Fatalf("compact=%t %s: got %v, want %q (%v)", compact, direction, text.Data, want, err)
			}
		}
	}
}

func TestByteSliceCopyAcrossConcurrentStorageOwners(t *testing.T) {
	left := newByteSliceHeaderValue("Slice<Uint8>", []byte("abcd"), 4, 4).Data.(*vmSlice)
	right := newByteSliceHeaderValue("Slice<Uint8>", []byte("wxyz"), 4, 4).Data.(*vmSlice)
	var group sync.WaitGroup
	for _, pair := range [][2]*vmSlice{{left, right}, {right, left}, {left, left}} {
		group.Go(func() {
			for range 100 {
				pair[0].copyBytesFrom(pair[1], 4)
			}
		})
	}
	group.Wait()
	for _, storage := range []*vmSlice{left, right} {
		if value := string(storage.bytes()); value != "abcd" && value != "wxyz" {
			t.Fatalf("copy corrupted bytes: %q", value)
		}
	}
}

func TestPackedByteSliceSharesViewAndSeparatesOwnedCopy(t *testing.T) {
	value := newByteSliceHeaderValue("Slice<Uint8>", []byte{1, 2, 3}, 3, 3)
	header := value.Data.(*vmSlice)
	view := newSliceViewValue("Slice<Uint8>", header, 1, 2, 2)
	if err := view.Data.(*vmSlice).setValueAt(0, newVMValue("Uint8", uint64(9))); err != nil {
		t.Fatal(err)
	}
	if got := header.ByteBacking[1]; got != 9 {
		t.Fatalf("shared byte = %d, want 9", got)
	}
	owned := newByteSliceHeaderValue("Slice<Uint8>", append([]byte(nil), header.ByteBacking...), 3, 3)
	if err := owned.Data.(*vmSlice).setValueAt(0, newVMValue("Uint8", uint64(7))); err != nil {
		t.Fatal(err)
	}
	if got := header.ByteBacking[0]; got != 1 {
		t.Fatalf("owned copy changed source byte to %d", got)
	}
}

func TestCompactByteSliceConversionPreservesValueSemantics(t *testing.T) {
	module := &moduleInstance{}
	first, err := convertStringToByteSlice(newVMValue("String", "go"), "Slice<Uint8>")
	if err != nil {
		t.Fatalf("convert first byte slice: %v", err)
	}
	second, err := convertStringToByteSlice(newVMValue("String", "go"), "Slice<Uint8>")
	if err != nil {
		t.Fatalf("convert second byte slice: %v", err)
	}
	if _, err := setIndexValue(module, first, newVMValue("Int", int64(0)), newVMValue("Uint8", uint64('n'))); err != nil {
		t.Fatalf("write first byte slice: %v", err)
	}

	firstText, _, err := module.convertByteSliceToString(first)
	if err != nil {
		t.Fatalf("convert first slice to string: %v", err)
	}
	secondText, _, err := module.convertByteSliceToString(second)
	if err != nil {
		t.Fatalf("convert second slice to string: %v", err)
	}
	if firstText.Data != "no" || secondText.Data != "go" {
		t.Fatalf("independent conversions changed together: first=%q second=%q", firstText.Data, secondText.Data)
	}
}

func TestCompactByteSliceHeadersShareBacking(t *testing.T) {
	module := &moduleInstance{}
	value, err := convertStringToByteSlice(newVMValue("String", "mini"), "Slice<Uint8>")
	if err != nil {
		t.Fatalf("convert byte slice: %v", err)
	}
	view, err := sliceValue(module, value, newVMValue("Int", int64(1)), newVMValue("Int", int64(3)), vmValue{})
	if err != nil {
		t.Fatalf("slice byte value: %v", err)
	}
	if _, err := setIndexValue(module, view, newVMValue("Int", int64(0)), newVMValue("Uint8", uint64('a'))); err != nil {
		t.Fatalf("write byte view: %v", err)
	}
	text, _, err := module.convertByteSliceToString(value)
	if err != nil {
		t.Fatalf("convert updated slice to string: %v", err)
	}
	if text.Data != "mani" {
		t.Fatalf("slice view did not share backing: got %q", text.Data)
	}
}

func TestAppendExpandedStringWritesDirectlyToByteSlice(t *testing.T) {
	module := &moduleInstance{}
	backing := make([]byte, 8)
	copy(backing, "go")
	target := newByteSliceHeaderValue("Slice<Uint8>", backing, 2, len(backing))

	result, err := appendValue(module, target, []vmValue{newVMValue("String", "mini")}, true)
	if err != nil {
		t.Fatalf("append expanded string: %v", err)
	}
	text, _, err := module.convertByteSliceToString(result)
	if err != nil {
		t.Fatalf("convert appended slice: %v", err)
	}
	if text.Data != "gomini" {
		t.Fatalf("append result = %q, want %q", text.Data, "gomini")
	}
	header := result.Data.(*vmSlice)
	if &header.ByteBacking[0] != &backing[0] {
		t.Fatal("append with spare capacity replaced the byte backing")
	}
	if target.Data.(*vmSlice).Len != 2 {
		t.Fatal("append changed the source slice header")
	}
}

func TestAppendToByteArraySlicePreservesBackingWithinCapacity(t *testing.T) {
	module := &moduleInstance{}
	for _, expand := range []bool{false, true} {
		array := newVMValue("Array<3, Uint8>", []vmValue{newVMValue("Uint8", uint64(1)), newVMValue("Uint8", uint64(0)), newVMValue("Uint8", uint64(0))})
		view, err := sliceValue(module, array, newVMValue("Int", int64(0)), newVMValue("Int", int64(1)), vmValue{})
		if err != nil {
			t.Fatal(err)
		}
		values := []vmValue{newVMValue("Uint8", uint64(2)), newVMValue("Uint8", uint64(3))}
		if expand {
			values = []vmValue{newVMValue("String", "\x02\x03")}
		}
		appended, err := appendValue(module, view, values, expand)
		if err != nil {
			t.Fatal(err)
		}
		for index, want := range []uint64{1, 2, 3} {
			got, err := numericAsUint64(array.Data.(*vmArray).valueAt(index))
			if err != nil || got != want {
				t.Fatalf("expand=%t, index=%d: got %d, want %d (%v)", expand, index, got, want, err)
			}
		}
		if err := appended.Data.(*vmSlice).setValueAt(0, newVMValue("Uint8", uint64(9))); err != nil {
			t.Fatal(err)
		}
		if got, _ := numericAsUint64(array.Data.(*vmArray).valueAt(0)); got != 9 {
			t.Fatalf("appended view detached from array: %d", got)
		}
	}
}
