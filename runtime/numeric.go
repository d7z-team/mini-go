package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/d7z-team/mini-go/compiler/types"
)

func zeroNumericValue(typ any) (vmValue, bool) {
	runtimeType := coerceRuntimeType(typ)
	info, ok := runtimeType.NumericInfo()
	if !ok {
		return vmValue{}, false
	}
	switch info.Kind {
	case types.NumericSigned:
		return newSignedVMValue(runtimeType, 0), true
	case types.NumericUnsigned:
		return newUnsignedVMValue(runtimeType, 0), true
	case types.NumericFloat:
		return newFloatVMValue(runtimeType, 0), true
	case types.NumericComplex:
		return vmValue{Type: runtimeType, Data: complex128(0)}, true
	default:
		return vmValue{}, false
	}
}

func decodeNumericConstant(typ any, raw json.RawMessage) (vmValue, bool, error) {
	runtimeType := coerceRuntimeType(typ)
	info, ok := runtimeType.NumericInfo()
	if !ok {
		return vmValue{}, false, nil
	}
	switch info.Kind {
	case types.NumericSigned:
		value, err := parseConstantInt64(raw)
		if err != nil {
			return vmValue{}, true, err
		}
		return normalizeSignedValue(runtimeType, value), true, nil
	case types.NumericUnsigned:
		value, err := parseConstantUint64(raw)
		if err != nil {
			return vmValue{}, true, err
		}
		return normalizeUnsignedValue(runtimeType, value), true, nil
	case types.NumericFloat:
		value, err := parseConstantFloat64(raw)
		if err != nil {
			return vmValue{}, true, err
		}
		return normalizeFloatValue(runtimeType, value), true, nil
	case types.NumericComplex:
		value, err := parseConstantComplex128(raw)
		if err != nil {
			return vmValue{}, true, err
		}
		return normalizeComplexValue(runtimeType, value), true, nil
	default:
		return vmValue{}, true, fmt.Errorf("unsupported numeric type %s", typ)
	}
}

func normalizeSignedValue(typ vmType, value int64) vmValue {
	if typ.Ref.Kind == types.Primitive {
		switch typ.Ref.Primitive {
		case types.PrimitiveInt8:
			value = int64(int8(value))
		case types.PrimitiveInt16:
			value = int64(int16(value))
		case types.PrimitiveInt32:
			value = int64(int32(value))
		}
		return newSignedVMValue(typ, value)
	}
	info, _ := typ.NumericInfo()
	switch info.Bits {
	case 8:
		value = int64(int8(value))
	case 16:
		value = int64(int16(value))
	case 32:
		value = int64(int32(value))
	}
	return newSignedVMValue(typ, value)
}

func normalizeUnsignedValue(typ vmType, value uint64) vmValue {
	if typ.Ref.Kind == types.Primitive {
		switch typ.Ref.Primitive {
		case types.PrimitiveUint8:
			value = uint64(uint8(value))
		case types.PrimitiveUint16:
			value = uint64(uint16(value))
		case types.PrimitiveUint32:
			value = uint64(uint32(value))
		}
		return newUnsignedVMValue(typ, value)
	}
	info, _ := typ.NumericInfo()
	switch info.Bits {
	case 8:
		value = uint64(uint8(value))
	case 16:
		value = uint64(uint16(value))
	case 32:
		value = uint64(uint32(value))
	}
	return newUnsignedVMValue(typ, value)
}

func normalizeFloatValue(typ vmType, value float64) vmValue {
	if typ.Ref.Kind == types.Primitive {
		if typ.Ref.Primitive == types.PrimitiveFloat32 {
			value = float64(float32(value))
		}
		return newFloatVMValue(typ, value)
	}
	info, _ := typ.NumericInfo()
	if info.Bits == 32 {
		value = float64(float32(value))
	}
	return newFloatVMValue(typ, value)
}

func normalizeComplexValue(typ vmType, value complex128) vmValue {
	if typ.Ref.Kind == types.Primitive {
		if typ.Ref.Primitive == types.PrimitiveComplex64 {
			value = complex(float64(float32(real(value))), float64(float32(imag(value))))
		}
		return vmValue{Type: typ, Data: value}
	}
	info, _ := typ.NumericInfo()
	if info.Bits == 64 {
		value = complex(float64(float32(real(value))), float64(float32(imag(value))))
	}
	return vmValue{Type: typ, Data: value}
}

func parseConstantInt64(raw json.RawMessage) (int64, error) {
	if text, ok := quotedConstantText(raw); ok {
		return strconv.ParseInt(text, 10, 64)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.ParseInt(number.String(), 10, 64)
	}
	return 0, fmt.Errorf("invalid signed integer constant %s", string(raw))
}

func parseConstantUint64(raw json.RawMessage) (uint64, error) {
	if text, ok := quotedConstantText(raw); ok {
		return strconv.ParseUint(text, 10, 64)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.ParseUint(number.String(), 10, 64)
	}
	return 0, fmt.Errorf("invalid unsigned integer constant %s", string(raw))
}

func parseConstantFloat64(raw json.RawMessage) (float64, error) {
	if text, ok := quotedConstantText(raw); ok {
		return strconv.ParseFloat(text, 64)
	}
	var out float64
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	if math.IsInf(out, 0) || math.IsNaN(out) {
		return 0, fmt.Errorf("non-finite float constant %s", string(raw))
	}
	return out, nil
}

func parseConstantComplex128(raw json.RawMessage) (complex128, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		return 0, fmt.Errorf("invalid complex constant %s", string(raw))
	}
	realPart, realOK := parseComplexComponent(fields["real"])
	imagPart, imagOK := parseComplexComponent(fields["imag"])
	if !realOK || !imagOK {
		return 0, fmt.Errorf("invalid complex constant %s", string(raw))
	}
	return complex(realPart, imagPart), nil
}

func parseComplexComponent(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, false
	}
	return value, true
}

func quotedConstantText(raw json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

func isNumericValue(value vmValue) bool {
	return value.scalarKind != 0 || isComplexValue(value)
}

func isIntegerValue(value vmValue) bool {
	return isSignedIntegerValue(value) || isUnsignedIntegerValue(value)
}

func isSignedIntegerValue(value vmValue) bool {
	_, ok := value.signedValue()
	return ok
}

func isUnsignedIntegerValue(value vmValue) bool {
	_, ok := value.unsignedValue()
	return ok
}

func isFloatValue(value vmValue) bool {
	_, ok := value.floatValue()
	return ok
}

func isComplexValue(value vmValue) bool {
	_, ok := value.payload().(complex128)
	return ok
}

func numericAsInt64(value vmValue) (int64, error) {
	if data, ok := value.signedValue(); ok {
		return data, nil
	}
	if data, ok := value.unsignedValue(); ok {
		return int64(data), nil
	}
	if data, ok := value.floatValue(); ok {
		return int64(data), nil
	}
	if data, ok := value.payload().(complex128); ok {
		return int64(real(data)), nil
	}
	return 0, fmt.Errorf("invalid %s numeric value", value.Type)
}

func numericAsUint64(value vmValue) (uint64, error) {
	if data, ok := value.signedValue(); ok {
		return uint64(data), nil
	}
	if data, ok := value.unsignedValue(); ok {
		return data, nil
	}
	if data, ok := value.floatValue(); ok {
		return uint64(data), nil
	}
	if data, ok := value.payload().(complex128); ok {
		return uint64(real(data)), nil
	}
	return 0, fmt.Errorf("invalid %s numeric value", value.Type)
}

func numericAsFloat64(value vmValue) (float64, error) {
	if data, ok := value.signedValue(); ok {
		return float64(data), nil
	}
	if data, ok := value.unsignedValue(); ok {
		return float64(data), nil
	}
	if data, ok := value.floatValue(); ok {
		return data, nil
	}
	return 0, fmt.Errorf("expected real numeric value, got %s", value.Type)
}

func numericAsComplex128(value vmValue) (complex128, error) {
	if data, ok := value.signedValue(); ok {
		return complex(float64(data), 0), nil
	}
	if data, ok := value.unsignedValue(); ok {
		return complex(float64(data), 0), nil
	}
	if data, ok := value.floatValue(); ok {
		return complex(data, 0), nil
	}
	if data, ok := value.payload().(complex128); ok {
		return data, nil
	}
	return 0, fmt.Errorf("expected numeric value, got %s", value.Type)
}
