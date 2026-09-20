package runtime

import (
	"errors"
	"sync"
	"testing"
)

func TestSequenceStorageViewsShareWritesAndSnapshotsOwnTheirBytes(t *testing.T) {
	for _, byteBacked := range []bool{false, true} {
		var value vmValue
		if byteBacked {
			value = newByteSliceValue("Slice<Uint8>", "abcd")
		} else {
			value = newSliceValue("Slice<Uint8>", []vmValue{
				newVMValue("Uint8", uint64('a')), newVMValue("Uint8", uint64('b')),
				newVMValue("Uint8", uint64('c')), newVMValue("Uint8", uint64('d')),
			})
		}
		source := value.Data.(*vmSlice)
		before := source.values()
		var group sync.WaitGroup
		for index := range 4 {
			view := newSliceViewValue(value.Type, source, index, 1, 1).Data.(*vmSlice)
			group.Go(func() {
				for range 100 {
					view.writeBytes(0, []byte{byte('A' + index)})
					for _, item := range source.values() {
						if _, err := numericAsUint64(item); err != nil {
							t.Error(err)
						}
					}
					if byteBacked {
						_ = source.bytes()
					}
				}
			})
		}
		group.Wait()
		for index := range 4 {
			old, _ := numericAsUint64(before[index])
			current, _ := numericAsUint64(source.valueAt(index))
			if old != uint64('a'+index) || current != uint64('A'+index) {
				t.Fatalf("backing=%v index=%d: old=%d current=%d", byteBacked, index, old, current)
			}
		}
	}
}

type storageEntropy func([]byte) (int, error)

func (read storageEntropy) Read(bytes []byte) (int, error) { return read(bytes) }

func TestEntropyWritesOnlyItsReportedPrefixWithoutHoldingStorageLock(t *testing.T) {
	value := newByteSliceValue("Slice<Uint8>", "abcd")
	storage := value.Data.(*vmSlice)
	machine := &vm{entropy: storageEntropy(func(bytes []byte) (int, error) {
		if got := string(storage.bytes()); got != "abcd" {
			t.Fatalf("input changed before delivery: %q", got)
		}
		copy(bytes, "WXYZ")
		return 2, errors.New("partial entropy failure")
	})}
	result, err := cryptoRandRead(intrinsicContext{vm: machine}, []vmValue{value})
	if err != nil || result[0].materializedData() != int64(2) || string(storage.bytes()) != "WXcd" {
		t.Fatalf("result=%v bytes=%q err=%v", result, storage.bytes(), err)
	}
}

func TestSliceCopyAndClearPreserveNestedArrayStorage(t *testing.T) {
	module := &moduleInstance{}
	array := newVMValue("Array<2, Int>", []vmValue{newVMValue("Int", int64(1)), newVMValue("Int", int64(2))})
	destination := newSliceValue("Slice<Array<2, Int>>", []vmValue{array})
	alias := array.Data.(*vmArray)
	source := newSliceValue(destination.Type, []vmValue{newVMValue(array.Type, []vmValue{newVMValue("Int", int64(3)), newVMValue("Int", int64(4))})})
	if _, err := copyValue(module, destination, source); err != nil {
		t.Fatal(err)
	}
	if alias.valueAt(1).materializedData() != int64(4) {
		t.Fatal("copy detached nested array storage")
	}
	if err := clearValue(module, destination); err != nil {
		t.Fatal(err)
	}
	if alias.valueAt(1).materializedData() != int64(0) {
		t.Fatal("clear detached nested array storage")
	}
}
