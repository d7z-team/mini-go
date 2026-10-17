package bytecode

import (
	"math"

	"github.com/d7z-team/mini-go/compiler/types"
)

// StorageCostVersion identifies logical managed storage accounting. These
// sizes describe guest allocations, independently of host layouts and reflect.
const StorageCostVersion = 1

const (
	StorageByteBytes          = 1
	StorageScalarBytes        = 8
	StorageReferenceBytes     = 8
	StorageDynamicBytes       = 24
	StorageStringBytes        = 24
	StorageSliceBytes         = 32
	StoragePointerBytes       = 24
	StorageRecordHeaderBytes  = 16
	StorageFrameHeaderBytes   = 32
	StorageControlRecordBytes = 64
	StorageMapBucketBytes     = 4
	StorageMapEntryBytes      = 16
	StorageMapHeaderBytes     = 32
	StorageChannelHeaderBytes = 24
)

// StorageCosts is generated into each runtime's contract. Metadata pages,
// collector scratch and nursery copy space belong to resident accounting.
type StorageCosts struct {
	Version       uint32 `json:"version"`
	Byte          uint64 `json:"byte"`
	Scalar        uint64 `json:"scalar"`
	Reference     uint64 `json:"reference"`
	Dynamic       uint64 `json:"dynamic"`
	String        uint64 `json:"string"`
	Slice         uint64 `json:"slice"`
	Pointer       uint64 `json:"pointer"`
	RecordHeader  uint64 `json:"record_header"`
	FrameHeader   uint64 `json:"frame_header"`
	ControlRecord uint64 `json:"control_record"`
	MapBucket     uint64 `json:"map_bucket"`
	MapEntry      uint64 `json:"map_entry"`
	MapHeader     uint64 `json:"map_header"`
	ChannelHeader uint64 `json:"channel_header"`
}

// CurrentStorageCosts returns the immutable accounting constants by value.
func CurrentStorageCosts() StorageCosts {
	return StorageCosts{
		Version: StorageCostVersion, Byte: StorageByteBytes,
		Scalar: StorageScalarBytes, Reference: StorageReferenceBytes,
		Dynamic: StorageDynamicBytes, String: StorageStringBytes,
		Slice: StorageSliceBytes, Pointer: StoragePointerBytes,
		RecordHeader: StorageRecordHeaderBytes, FrameHeader: StorageFrameHeaderBytes,
		ControlRecord: StorageControlRecordBytes,
		MapBucket:     StorageMapBucketBytes, MapEntry: StorageMapEntryBytes,
		MapHeader: StorageMapHeaderBytes, ChannelHeader: StorageChannelHeaderBytes,
	}
}

// PrimitiveStorageBytes returns logical inline widths indexed by the shared
// primitive kind. Aggregates sum their field widths without host padding;
// descriptors count only themselves, not separately allocated backing data.
func PrimitiveStorageBytes() []uint64 {
	widths := make([]uint64, int(types.PrimitiveFunction)+1)
	for primitive := types.PrimitiveBool; primitive <= types.PrimitiveFunction; primitive++ {
		switch primitive {
		case types.PrimitiveBool:
			widths[primitive] = StorageByteBytes
		case types.PrimitiveString:
			widths[primitive] = StorageStringBytes
		case types.PrimitiveError:
			widths[primitive] = StorageDynamicBytes
		case types.PrimitiveFunction:
			widths[primitive] = StorageReferenceBytes
		default:
			if info, ok := types.NumericTypeInfo(nil, types.Builtin(primitive)); ok {
				widths[primitive] = uint64(info.Bits / 8)
			}
		}
	}
	return widths
}

// CheckedStorageBytes computes header + count*stride in the common signed
// 64-bit accounting range. Overflow is rejected before reserving storage.
func CheckedStorageBytes(header, count, stride uint64) (uint64, bool) {
	if header > math.MaxInt64 || stride != 0 && count > (math.MaxInt64-header)/stride {
		return 0, false
	}
	return header + count*stride, true
}
