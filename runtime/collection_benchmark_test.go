package runtime

import (
	"strconv"
	"testing"
)

func BenchmarkByteSliceCopy(b *testing.B) {
	for _, size := range []int{256, 8192} {
		for _, sourceKind := range []string{"slice", "string", "overlap"} {
			b.Run(sourceKind+"/"+strconv.Itoa(size), func(b *testing.B) {
				module := &moduleInstance{}
				backing := make([]byte, size+1)
				for i := range backing {
					backing[i] = byte(i)
				}
				source := newByteSliceHeaderValue("Slice<Uint8>", backing, size, size+1)
				destination := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, size), size, size)
				if sourceKind == "string" {
					source = newVMValue("String", string(backing[:size]))
				} else if sourceKind == "overlap" {
					destination = newSliceViewValue("Slice<Uint8>", source.Data.(*vmSlice), 1, size, size)
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					copied, err := copyValue(module, destination, source)
					if err != nil || copied.scalar != uint64(size) {
						b.Fatalf("copy: %v, %v", copied, err)
					}
				}
			})
		}
	}
}
