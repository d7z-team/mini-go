package runtime

import (
	"fmt"
	"math"

	"github.com/d7z-team/mini-go/compiler/types"
)

type vmScalarKind uint8

const (
	vmScalarSigned vmScalarKind = iota + 1
	vmScalarUnsigned
	vmScalarFloat
)

type vmValue struct {
	Type        vmType
	Data        any
	ref         guestRef
	offset      uint32
	heap        *managedHeap
	owner       *vm
	module      *moduleInstance
	layouts     *preparedLayoutRegistry
	scalar      uint64
	scalarKind  vmScalarKind
	inlineOwner *frame
	inlineIndex int
	inlineTemp  bool
}

// payload exposes only detached data and scalar lanes. Published aggregate
// values are decoded by their prepared typed view; they must never resolve to
// an owning host object through a generic heap lookup.
func (value vmValue) payload() any {
	if !value.ref.valid() {
		return value.Data
	}
	if value.scalarKind != 0 {
		return value.materializedData()
	}
	switch value.Type.ShapeKind() {
	case types.Primitive:
		if boolean, ok := value.boolValue(); ok {
			return boolean
		}
		if _, ok := value.Type.NumericInfo(); ok {
			decoded := decodeManagedInline(value.heap, value.ref, value.offset, value.Type, preparedValueLayout{}, value.layouts, value.owner)
			return decoded.materializedData()
		}
		if text, ok := materializeManagedString(value); ok {
			return text
		}
		if value.Type.Primitive(types.PrimitiveFunction) {
			if function, ok := materializeManagedFunction(value); ok {
				return function
			}
		}
	case types.Any, types.Interface:
		if inner, ok := materializeManagedDynamic(value); ok {
			return inner
		}
	case types.Slice:
		if slice, ok := materializeManagedSlice(value); ok {
			return slice
		}
	case types.Array:
		if array, ok := materializeManagedArray(value); ok {
			return array
		}
	case types.Struct:
		if structure, ok := materializeManagedStruct(value); ok {
			return structure
		}
	case types.Pointer:
		if pointer, ok := materializeManagedPointer(value); ok {
			return pointer
		}
	case types.Function:
		if function, ok := materializeManagedFunction(value); ok {
			return function
		}
	case types.Map:
		if managedMap, ok := materializeManagedMap(value); ok {
			return managedMap
		}
	case types.Waitable:
		if waitable, ok := materializeManagedWaitable(value); ok {
			if waitable.mutex != nil {
				return waitable.mutex
			}
			return waitable
		}
	}
	return nil
}

func (value *vmValue) setPayload(payload any) {
	if value == nil {
		return
	}
	if value.ref.valid() {
		// Published values are mutated through their prepared page layout.
		return
	}
	if owner := value.inlineOwner; owner != nil {
		if structure, ok := payload.(*vmStruct); ok && structure != nil && structure.inlineRaw != nil {
			value.Data = payload
			return
		}
		if array, ok := payload.(*vmArray); ok && array != nil && array.inlineRaw != nil {
			value.Data = payload
			return
		}
		if payload == value.Data {
			return
		}
		updated := *value
		updated.Data = payload
		var err error
		if value.inlineTemp {
			err = owner.writeTempValue(value.inlineIndex, updated)
		} else {
			err = owner.writeInlineLocal(value.inlineIndex, updated)
		}
		if err != nil {
			owner.recordStorageFailure(fmt.Errorf("update inline aggregate: %w", err))
		}
		return
	}
	value.Data = payload
}

func (m *moduleInstance) detachedValue(value vmValue, payload any) vmValue {
	value.ref, value.heap, value.offset = guestRef{}, nil, 0
	value.inlineOwner, value.inlineIndex = nil, 0
	value.Data = payload
	return m.adoptValue(value)
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
			vmSliceStorage: newValueSliceStorage(data), Len: len(data), Cap: len(data),
		}}}
	case bool:
		var scalar uint64
		if data {
			scalar = 1
		}
		return vmValue{Type: runtimeType, Data: data, scalar: scalar}
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
	case complex64:
		return vmValue{Type: runtimeType, Data: complex128(data)}
	case complex128:
		return vmValue{Type: runtimeType, Data: data}
	}
	return vmValue{Type: runtimeType, Data: data}
}

func newBoolValue(value bool) vmValue {
	var scalar uint64
	if value {
		scalar = 1
	}
	return vmValue{Type: boolRuntimeType, Data: value, scalar: scalar}
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

// boolValue reads the packed boolean lane before consulting the detached
// boundary payload. Boolean values are ubiquitous in branch and comparison
// opcodes, so they must not resolve a managed record or allocate a boxed bool
// just to decide a branch.
func (value vmValue) boolValue() (bool, bool) {
	if value.Type.Primitive(types.PrimitiveBool) {
		return value.scalar != 0, true
	}
	boolean, ok := value.Data.(bool)
	return boolean, ok
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
		// Prepared dynamic frame lanes retain the declared Any/interface
		// wrapper while storing the concrete value as a nested vmValue.  Debug
		// and reflection boundaries expose the concrete materialized payload;
		// otherwise a typed nil would be reported as an opaque vmValue.
		if decoded, ok := value.Data.(vmValue); ok {
			return decoded.materializedData()
		}
		return value.payload()
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
