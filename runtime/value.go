package runtime

import "math"

type vmScalarKind uint8

const (
	vmScalarSigned vmScalarKind = iota + 1
	vmScalarUnsigned
	vmScalarFloat
)

type vmValue struct {
	Type       vmType
	Data       any
	scalar     uint64
	scalarKind vmScalarKind
}

var vmScalarData = &struct{}{}

func newVMValue(typ, data any) vmValue {
	runtimeType, ok := typ.(vmType)
	if !ok {
		runtimeType = coerceRuntimeType(typ)
	}
	switch data := data.(type) {
	case []vmValue:
		return vmValue{Type: runtimeType, Data: &vmArray{vmSlice: vmSlice{
			vmSliceStorage: &vmSliceStorage{Backing: data}, Len: len(data), Cap: len(data),
		}}}
	case int:
		return newSignedVMValue(runtimeType, int64(data))
	case int8:
		return newSignedVMValue(runtimeType, int64(data))
	case int16:
		return newSignedVMValue(runtimeType, int64(data))
	case int32:
		return newSignedVMValue(runtimeType, int64(data))
	case int64:
		return newSignedVMValue(runtimeType, data)
	case uint:
		return newUnsignedVMValue(runtimeType, uint64(data))
	case uint8:
		return newUnsignedVMValue(runtimeType, uint64(data))
	case uint16:
		return newUnsignedVMValue(runtimeType, uint64(data))
	case uint32:
		return newUnsignedVMValue(runtimeType, uint64(data))
	case uint64:
		return newUnsignedVMValue(runtimeType, data)
	case float32:
		return newFloatVMValue(runtimeType, float64(data))
	case float64:
		return newFloatVMValue(runtimeType, data)
	}
	return vmValue{Type: runtimeType, Data: data}
}

func newBoolValue(value bool) vmValue {
	return vmValue{Type: boolRuntimeType, Data: value}
}

func newSignedVMValue(typ vmType, value int64) vmValue {
	return vmValue{Type: typ, Data: vmScalarData, scalar: uint64(value), scalarKind: vmScalarSigned}
}

func newUnsignedVMValue(typ vmType, value uint64) vmValue {
	return vmValue{Type: typ, Data: vmScalarData, scalar: value, scalarKind: vmScalarUnsigned}
}

func newFloatVMValue(typ vmType, value float64) vmValue {
	return vmValue{Type: typ, Data: vmScalarData, scalar: math.Float64bits(value), scalarKind: vmScalarFloat}
}

func (value vmValue) signedValue() (int64, bool) {
	return int64(value.scalar), value.scalarKind == vmScalarSigned
}

func (value vmValue) unsignedValue() (uint64, bool) {
	return value.scalar, value.scalarKind == vmScalarUnsigned
}

func (value vmValue) floatValue() (float64, bool) {
	return math.Float64frombits(value.scalar), value.scalarKind == vmScalarFloat
}

// materializedData exposes scalar values at presentation and reflection
// boundaries. The instruction loop uses the typed accessors above so numeric
// results remain allocation-free.
func (value vmValue) materializedData() any {
	switch value.scalarKind {
	case vmScalarSigned:
		return int64(value.scalar)
	case vmScalarUnsigned:
		return value.scalar
	case vmScalarFloat:
		return math.Float64frombits(value.scalar)
	default:
		return value.Data
	}
}

func (m *moduleInstance) qualifyValuesForExport(values []vmValue) []vmValue {
	return values
}

func (m *moduleInstance) qualifyValuesForArgumentBoundary(values []vmValue) []vmValue {
	return values
}

func (m *moduleInstance) qualifyValueForArgumentBoundary(value vmValue) vmValue {
	return value
}

func (m *moduleInstance) qualifyValueForExport(value vmValue) vmValue {
	return value
}
