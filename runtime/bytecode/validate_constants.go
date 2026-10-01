package bytecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func validateConstantValue(path string, constant Constant, table *types.TypeTable, modulePath string) error {
	raw := bytes.TrimSpace(constant.Value)
	view := types.View(table, constant.Type)
	if unresolvedNamedType(constant.Type, table) {
		if named := constant.Type.Named; named.ModulePath == "" || named.ModulePath == modulePath {
			return newValidationError(path, errors.New("constant type has unresolved local named type"))
		}
		return nil
	}
	if bytes.Equal(raw, []byte("null")) {
		if view.Nilable() {
			return nil
		}
		return newValidationError(path, errors.New("null constant requires nil-able type"))
	}
	if constant.Type.Kind == types.Any {
		return nil
	}
	if primitive, ok := view.Primitive(); ok {
		switch primitive {
		case types.PrimitiveBool:
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return newValidationError(path, errors.New("Bool constant requires JSON boolean"))
			}
			return nil
		case types.PrimitiveString:
			if _, err := DecodeStringConstant(raw); err != nil {
				return newValidationError(path, err)
			}
			return nil
		}
	}
	if info, ok := view.NumericInfo(); ok {
		if constant.Untyped {
			return validateUntypedNumericConstantValue(path, raw, info)
		}
		return validateNumericConstantValue(path, raw, info)
	}
	if view.Shape() == types.Slice {
		elem, ok := view.Elem()
		primitive, primitiveOK := types.View(table, elem).Primitive()
		if ok && primitiveOK && primitive == types.PrimitiveUint8 {
			var encoded string
			if err := json.Unmarshal(raw, &encoded); err != nil {
				return newValidationError(path, errors.New("byte slice constant requires canonical base64 JSON string"))
			}
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || base64.StdEncoding.EncodeToString(decoded) != encoded {
				return newValidationError(path, errors.New("byte slice constant requires canonical base64 JSON string"))
			}
			return nil
		}
	}
	return unsupportedValueValidationError(path, errors.New("constant value is unsupported for non-scalar type"))
}

func unresolvedNamedType(ref types.TypeRef, table *types.TypeTable) bool {
	if ref.Kind != types.Named {
		return false
	}
	if ref.Node != "" {
		_, ok := table.Node(ref)
		return !ok
	}
	_, ok := table.Named(ref.Named)
	return !ok
}

func validateUntypedNumericConstantValue(path string, raw []byte, info types.NumericInfo) error {
	switch info.Kind {
	case types.NumericSigned:
		text, ok := constantNumberText(raw)
		if !ok || !integerLiteralText(text) {
			return newValidationError(path, errors.New("untyped integer constant requires decimal integer"))
		}
		return nil
	case types.NumericUnsigned:
		text, ok := constantNumberText(raw)
		if !ok || strings.HasPrefix(strings.TrimSpace(text), "-") || !integerLiteralText(text) {
			return newValidationError(path, errors.New("untyped unsigned integer constant requires non-negative decimal integer"))
		}
		return nil
	case types.NumericFloat:
		if !exactRationalConstantValue(raw) {
			return newValidationError(path, errors.New("untyped float constant requires canonical exact rational text"))
		}
		return nil
	case types.NumericComplex:
		if !exactComplexConstantValue(raw) {
			return newValidationError(path, errors.New("untyped complex constant requires canonical exact components"))
		}
		return nil
	default:
		return unsupportedValueValidationError(path, errors.New("unsupported untyped numeric constant type"))
	}
}

func exactRationalConstantValue(raw []byte) bool {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return false
	}
	parts := strings.Split(text, "/")
	if len(parts) != 2 || !canonicalSignedIntegerText(parts[0]) || !canonicalUnsignedIntegerText(parts[1]) || parts[1] == "0" {
		return false
	}
	return parts[0] != "0" || parts[1] == "1"
}

func exactComplexConstantValue(raw []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		return false
	}
	realPart, realOK := fields["real"]
	imagPart, imagOK := fields["imag"]
	return realOK && imagOK && exactRationalConstantValue(realPart) && exactRationalConstantValue(imagPart)
}

func canonicalSignedIntegerText(text string) bool {
	if strings.HasPrefix(text, "-") {
		return len(text) > 1 && text[1] != '0' && canonicalUnsignedIntegerText(text[1:])
	}
	return canonicalUnsignedIntegerText(text)
}

func canonicalUnsignedIntegerText(text string) bool {
	if text == "" || len(text) > 1 && text[0] == '0' {
		return false
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func validateNumericConstantValue(path string, raw []byte, info types.NumericInfo) error {
	switch info.Kind {
	case types.NumericSigned:
		return validateSignedConstantValue(path, raw, info.Bits)
	case types.NumericUnsigned:
		return validateUnsignedConstantValue(path, raw, info.Bits)
	case types.NumericFloat:
		return validateFloatConstantValue(path, raw, info.Bits)
	case types.NumericComplex:
		return validateComplexConstantValue(path, raw, info.Bits)
	default:
		return unsupportedValueValidationError(path, errors.New("unsupported numeric constant type"))
	}
}

func validateSignedConstantValue(path string, raw []byte, bits int) error {
	text, ok := constantNumberText(raw)
	if !ok {
		return newValidationError(path, errors.New("signed integer constant requires JSON integer"))
	}
	if strings.ContainsAny(text, ".eE") {
		return newValidationError(path, errors.New("signed integer constant requires integral value"))
	}
	if _, err := strconv.ParseInt(text, 10, bits); err != nil {
		return outOfRangeValidationError(path, fmt.Errorf("signed integer constant out of range for %d-bit type", bits))
	}
	return nil
}

func validateUnsignedConstantValue(path string, raw []byte, bits int) error {
	text, ok := constantNumberText(raw)
	if !ok {
		return newValidationError(path, errors.New("unsigned integer constant requires JSON integer"))
	}
	if strings.HasPrefix(strings.TrimSpace(text), "-") || strings.ContainsAny(text, ".eE") {
		return newValidationError(path, errors.New("unsigned integer constant requires non-negative integral value"))
	}
	if _, err := strconv.ParseUint(text, 10, bits); err != nil {
		return outOfRangeValidationError(path, fmt.Errorf("unsigned integer constant out of range for %d-bit type", bits))
	}
	return nil
}

func validateFloatConstantValue(path string, raw []byte, bits int) error {
	value, ok := constantFloat64(raw)
	if !ok || math.IsInf(value, 0) || math.IsNaN(value) {
		return newValidationError(path, errors.New("float constant requires finite JSON number"))
	}
	if bits == 32 && math.IsInf(float64(float32(value)), 0) {
		return outOfRangeValidationError(path, errors.New("float constant out of range for Float32"))
	}
	return nil
}

func validateComplexConstantValue(path string, raw []byte, bits int) error {
	realPart, imagPart, ok := constantComplex128(raw)
	if !ok || math.IsInf(realPart, 0) || math.IsNaN(realPart) || math.IsInf(imagPart, 0) || math.IsNaN(imagPart) {
		return newValidationError(path, errors.New("complex constant requires finite real and imaginary components"))
	}
	if bits == 64 && (math.IsInf(float64(float32(realPart)), 0) || math.IsInf(float64(float32(imagPart)), 0)) {
		return outOfRangeValidationError(path, errors.New("complex constant out of range for Complex64"))
	}
	return nil
}

func constantNumberText(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", false
		}
		text = strings.TrimSpace(text)
		return text, text != ""
	}
	text = string(raw)
	if !jsonNumberLiteralText(text) {
		return "", false
	}
	return text, true
}

func constantFloat64(raw []byte) (float64, bool) {
	text, ok := constantNumberText(raw)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseFloat(text, 64)
	return value, err == nil
}

func integerLiteralText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "-") {
		text = strings.TrimPrefix(text, "-")
		if text == "" {
			return false
		}
	}
	for _, ch := range text {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func jsonNumberLiteralText(text string) bool {
	if text == "" {
		return false
	}
	i := 0
	if text[i] == '-' {
		i++
		if i == len(text) {
			return false
		}
	}
	if text[i] == '0' {
		i++
	} else if text[i] >= '1' && text[i] <= '9' {
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
		}
	} else {
		return false
	}
	if i < len(text) && text[i] == '.' {
		i++
		start := i
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
		i++
		if i < len(text) && (text[i] == '+' || text[i] == '-') {
			i++
		}
		start := i
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(text)
}

func constantComplex128(raw []byte) (float64, float64, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 {
		return 0, 0, false
	}
	realRaw, realOK := fields["real"]
	imagRaw, imagOK := fields["imag"]
	if !realOK || !imagOK {
		return 0, 0, false
	}
	realPart, realOK := jsonFloat64(realRaw)
	imagPart, imagOK := jsonFloat64(imagRaw)
	return realPart, imagPart, realOK && imagOK
}

func jsonFloat64(raw []byte) (float64, bool) {
	text := strings.TrimSpace(string(raw))
	if !jsonNumberLiteralText(text) {
		return 0, false
	}
	value, err := strconv.ParseFloat(text, 64)
	return value, err == nil
}
