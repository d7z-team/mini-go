package bytecode

import (
	"math"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestPrimitiveStorageCountsSemanticWidthWithoutHostPadding(t *testing.T) {
	widths := PrimitiveStorageBytes()
	for _, test := range []struct {
		kind types.PrimitiveKind
		want uint64
	}{
		{types.PrimitiveBool, 1},
		{types.PrimitiveInt8, 1},
		{types.PrimitiveUint8, 1},
		{types.PrimitiveInt16, 2},
		{types.PrimitiveUint16, 2},
		{types.PrimitiveInt32, 4},
		{types.PrimitiveUint32, 4},
		{types.PrimitiveFloat32, 4},
		{types.PrimitiveInt, 8},
		{types.PrimitiveInt64, 8},
		{types.PrimitiveUint, 8},
		{types.PrimitiveUint64, 8},
		{types.PrimitiveUintptr, 8},
		{types.PrimitiveFloat64, 8},
		{types.PrimitiveComplex64, 8},
		{types.PrimitiveComplex128, 16},
		{types.PrimitiveString, 24},
		{types.PrimitiveError, 24},
		{types.PrimitiveFunction, 8},
	} {
		if got := widths[test.kind]; got != test.want {
			t.Errorf("primitive %v storage = %d, want %d", test.kind, got, test.want)
		}
	}
}

func TestStorageCapacityAdmissionArithmetic(t *testing.T) {
	for _, test := range []struct {
		name                  string
		header, count, stride uint64
		want                  uint64
		valid                 bool
	}{
		{"empty backing", StorageRecordHeaderBytes, 0, StorageReferenceBytes, StorageRecordHeaderBytes, true},
		{"reference backing", StorageRecordHeaderBytes, 7, StorageReferenceBytes, 72, true},
		{"zero sized elements", StorageRecordHeaderBytes, math.MaxUint64, 0, StorageRecordHeaderBytes, true},
		{"exact limit", 7, (math.MaxInt64 - 7) / 8, 8, math.MaxInt64, true},
		{"multiply overflow", 0, math.MaxUint64, 8, 0, false},
		{"header overflow", math.MaxUint64, 0, 0, 0, false},
		{"addition overflow", 8, (math.MaxInt64 - 7) / 8, 8, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, valid := CheckedStorageBytes(test.header, test.count, test.stride)
			if got != test.want || valid != test.valid {
				t.Fatalf("cost = (%d, %t), want (%d, %t)", got, valid, test.want, test.valid)
			}
		})
	}
}
