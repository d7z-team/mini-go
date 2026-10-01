// Package constant implements exact compile-time Mini-Go values and arithmetic.
package constant

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

// Value is a source-level exact fact. Text is a canonical arbitrary precision
// number or raw string bytes and Type is the source type identity.
type Value struct {
	Text            string
	Type            string
	Untyped         bool
	IsString        bool `json:",omitempty"`
	Real            string
	Imag            string
	canonicalNumber string
	numeratorEnd    int
}

// Rational returns the exact numeric value. Constructor-owned facts are tied
// to their immutable text; edited or decoded values are parsed normally.
func (value Value) Rational() (Rational, bool) {
	if value.IsString {
		return Rational{}, false
	}
	if value.canonicalNumber != "" && value.Text == value.canonicalNumber {
		if value.numeratorEnd != 0 {
			return Rational{Numerator: value.Text[:value.numeratorEnd], Denominator: value.Text[value.numeratorEnd+1:]}, true
		}
		return Rational{Numerator: value.Text, Denominator: "1"}, true
	}
	return ParseRationalLiteral(value.Text)
}

// RoundFloat rounds a numeric fact to an IEEE float32 or float64 value while
// preserving its source type and retaining the resulting exact rational.
func (value Value) RoundFloat(bits int) (Value, bool) {
	number, valid := value.Rational()
	if !valid || bits != 32 && bits != 64 {
		return Value{}, false
	}
	rounded, valid := RoundRationalFloat(number, bits)
	if !valid {
		return Value{}, false
	}
	return numericFact(rounded, value.Type, value.Untyped), true
}

func numericFact(number Rational, typ string, untyped bool) Value {
	text := number.String()
	value := Value{Text: text, Type: typ, Untyped: untyped, canonicalNumber: text}
	if number.Denominator != "1" {
		value.numeratorEnd = len(number.Numerator)
	}
	return value
}

type Kind uint8

const (
	Invalid Kind = iota
	Boolean
	Number
	StringValue
	ComplexValue
	NilValue
)

// Kind distinguishes exact values independently of their declared type identity.
func (value Value) Kind() Kind {
	if value.IsString {
		return StringValue
	}
	if value.Text == "null" {
		return NilValue
	}
	if value.Imag != "" {
		return ComplexValue
	}
	if value.Text == "true" || value.Text == "false" {
		return Boolean
	}
	if value.Text != "" {
		return Number
	}
	return Invalid
}

// String constructs an exact string value without quoting or copying its bytes.
func String(text, typ string, untyped bool) Value {
	return Value{Text: text, Type: typ, Untyped: untyped, IsString: true}
}

// Ref returns an independently owned fact for a literal node. Published facts
// are read-only; a conversion constructs a new fact instead of editing it.
func (value Value) Ref() *Value { return &value }

// Scalar constructs a compiler-produced boolean, nil, or numeric spelling.
// Source input must first pass the literal parser or exact numeric constructors.
func Scalar(text string) *Value { return &Value{Text: text} }

func (value Value) StringValue() (string, bool) { return value.Text, value.IsString }

// ExactText formats a source/display boundary while retaining raw bytes internally.
func (value Value) ExactText() string {
	if value.IsString {
		return strconv.Quote(value.Text)
	}
	return value.Text
}

// Integer constructs an exact integer value from signed decimal text.
func Integer(text, typ string, untyped bool) (Value, bool) {
	text, ok := NormalizeSignedDecimal(text)
	if !ok {
		return Value{}, false
	}
	return numericFact(Rational{Numerator: text, Denominator: "1"}, strings.TrimSpace(typ), untyped), true
}

// Numeric canonicalizes an exact integer or rational value.
func Numeric(text, typ string, untyped bool) (Value, bool) {
	value, ok := ParseRationalLiteral(text)
	if !ok {
		return Value{}, false
	}
	return numericFact(value, strings.TrimSpace(typ), untyped), true
}

// FromJSON decodes the numeric representation used at compiler artifact
// boundaries.
func FromJSON(raw json.RawMessage, typ string, untyped bool) (Value, bool) {
	if typ == "Bool" {
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return Value{}, false
		}
		return Value{Text: strconv.FormatBool(value), Type: typ, Untyped: untyped}, true
	}
	if typ == "Complex64" || typ == "Complex128" {
		var parts struct {
			Real json.RawMessage `json:"real"`
			Imag json.RawMessage `json:"imag"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return Value{}, false
		}
		realPart, realOK := FromJSON(parts.Real, "Float64", true)
		imaginaryPart, imagOK := FromJSON(parts.Imag, "Float64", true)
		return Value{Real: realPart.Text, Imag: imaginaryPart.Text, Type: typ, Untyped: untyped}, realOK && imagOK
	}
	var text string
	if typ == "String" {
		var err error
		text, err = bytecode.DecodeStringConstant(raw)
		if err != nil {
			return Value{}, false
		}
		return String(text, typ, untyped), true
	}
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return Value{}, false
		}
	} else {
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return Value{}, false
		}
		text = number.String()
	}
	return Numeric(text, typ, untyped)
}

// ParseIntegerLiteral parses a Go integer literal into an untyped exact value.
func ParseIntegerLiteral(text string) (Value, bool) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "_", ""))
	if text == "" {
		return Value{}, false
	}
	sign := ""
	if text[0] == '+' || text[0] == '-' {
		if text[0] == '-' {
			sign = "-"
		}
		text = text[1:]
	}
	if text == "" {
		return Value{}, false
	}
	base, digits := 10, text
	if len(text) > 1 && text[0] == '0' {
		switch text[1] {
		case 'b', 'B':
			base, digits = 2, text[2:]
		case 'o', 'O':
			base, digits = 8, text[2:]
		case 'x', 'X':
			base, digits = 16, text[2:]
		default:
			base, digits = 8, text[1:]
		}
	}
	if digits == "" {
		if text == "0" {
			return numericFact(Rational{Numerator: "0", Denominator: "1"}, "Int", true), true
		}
		return Value{}, false
	}
	if base == 10 {
		for i := 0; i < len(digits); i++ {
			if digits[i] < '0' || digits[i] > '9' {
				return Value{}, false
			}
		}
		if digits == "0" {
			sign = ""
		}
		return numericFact(Rational{Numerator: sign + digits, Denominator: "1"}, "Int", true), true
	}
	firstDigit := integerDigit(rune(digits[0]))
	if firstDigit < 0 || firstDigit >= base {
		return Value{}, false
	}
	if parsed, err := strconv.ParseUint(digits, base, 64); err == nil {
		value := strconv.FormatUint(parsed, 10)
		if parsed != 0 {
			value = sign + value
		}
		return numericFact(Rational{Numerator: value, Denominator: "1"}, "Int", true), true
	}
	value := "0"
	for _, digit := range digits {
		n := integerDigit(digit)
		if n < 0 || n >= base {
			return Value{}, false
		}
		value, _ = MultiplySignedDecimal(value, strconv.Itoa(base))
		value, _ = AddSignedDecimal(value, strconv.Itoa(n))
	}
	value, ok := NormalizeSignedDecimal(sign + value)
	return numericFact(Rational{Numerator: value, Denominator: "1"}, "Int", true), ok
}

// Unary applies a constant unary operator.
func Unary(operator string, value Value) (Value, bool) {
	if value.IsString {
		return Value{}, false
	}
	if value.Imag != "" {
		if operator == "+" {
			return value, true
		}
		if operator != "-" {
			return Value{}, false
		}
		realPart, realOK := ParseRationalLiteral(value.Real)
		imaginaryPart, imagOK := ParseRationalLiteral(value.Imag)
		realPart, _ = NegateRational(realPart)
		imaginaryPart, _ = NegateRational(imaginaryPart)
		value.Real, value.Imag = realPart.String(), imaginaryPart.String()
		return value, realOK && imagOK
	}
	if operator == "!" && (value.Text == "true" || value.Text == "false") {
		value.Text = strconv.FormatBool(value.Text == "false")
		return value, true
	}
	if operator == "+" || operator == "-" {
		rational, valid := value.Rational()
		if valid {
			if operator == "-" {
				rational, valid = NegateRational(rational)
			}
			return numericFact(rational, value.Type, value.Untyped), valid
		}
	}
	var out string
	var ok bool
	switch operator {
	case "+":
		out, ok = NormalizeSignedDecimal(value.Text)
	case "-":
		out, ok = NegateSignedDecimal(value.Text)
	case "^":
		negated, valid := NegateSignedDecimal(value.Text)
		if valid {
			out, ok = SubtractSignedDecimal(negated, "1")
		}
	}
	if !ok {
		return Value{}, false
	}
	return numericFact(Rational{Numerator: out, Denominator: "1"}, value.Type, value.Untyped), true
}

// Binary applies a constant binary operator.
func Binary(operator string, left, right Value) (Value, bool) {
	if left.Kind() == Boolean && right.Kind() == Boolean {
		var value bool
		switch operator {
		case "&&":
			value = left.Text == "true" && right.Text == "true"
		case "||":
			value = left.Text == "true" || right.Text == "true"
		case "==":
			value = left.Text == right.Text
		case "!=":
			value = left.Text != right.Text
		default:
			return Value{}, false
		}
		if operator == "==" || operator == "!=" {
			return Value{Text: strconv.FormatBool(value), Type: "Bool", Untyped: true}, true
		}
		if left.Untyped && !right.Untyped {
			left.Type = right.Type
		}
		left.Text = strconv.FormatBool(value)
		left.Untyped = left.Untyped && right.Untyped
		return left, true
	}
	if operator == "==" || operator == "!=" || operator == "<" || operator == "<=" || operator == ">" || operator == ">=" {
		comparison := 0
		if left.Kind() == ComplexValue || right.Kind() == ComplexValue {
			if operator != "==" && operator != "!=" {
				return Value{}, false
			}
			difference, ok := complexBinary("-", left, right)
			if !ok {
				return Value{}, false
			}
			r, rok := ParseRationalLiteral(difference.Real)
			i, iok := ParseRationalLiteral(difference.Imag)
			if !rok || !iok {
				return Value{}, false
			}
			if r.Numerator != "0" || i.Numerator != "0" {
				comparison = 1
			}
		} else if left.Kind() == StringValue && right.Kind() == StringValue {
			comparison = strings.Compare(left.Text, right.Text)
		} else {
			l, lok := left.Rational()
			r, rok := right.Rational()
			if !lok || !rok {
				return Value{}, false
			}
			comparison = CompareRational(l, r)
		}
		value := false
		switch operator {
		case "==":
			value = comparison == 0
		case "!=":
			value = comparison != 0
		case "<":
			value = comparison < 0
		case "<=":
			value = comparison <= 0
		case ">":
			value = comparison > 0
		case ">=":
			value = comparison >= 0
		}
		return Value{Text: strconv.FormatBool(value), Type: "Bool", Untyped: true}, true
	}
	if left.Imag != "" || right.Imag != "" {
		return complexBinary(operator, left, right)
	}
	if operator == "+" && left.IsString && right.IsString {
		if left.Untyped && !right.Untyped {
			left.Type = right.Type
		}
		return String(left.Text+right.Text, left.Type, left.Untyped && right.Untyped), true
	}
	if (operator == "+" || operator == "-" || operator == "*" || operator == "/") &&
		(!integerType(left.Type) || !integerType(right.Type)) {
		leftValue, leftOK := left.Rational()
		rightValue, rightOK := right.Rational()
		if !leftOK || !rightOK {
			return Value{}, false
		}
		var result Rational
		var ok bool
		switch operator {
		case "+":
			result, ok = AddRational(leftValue, rightValue)
		case "-":
			result, ok = SubtractRational(leftValue, rightValue)
		case "*":
			result, ok = MultiplyRational(leftValue, rightValue)
		case "/":
			result, ok = DivideRational(leftValue, rightValue)
		}
		if !ok {
			return Value{}, false
		}
		if integerType(left.Type) && !integerType(right.Type) {
			left.Type = right.Type
		}
		left.Untyped = left.Untyped && right.Untyped
		return numericFact(result, left.Type, left.Untyped), true
	}
	var out string
	var ok bool
	switch operator {
	case "+":
		out, ok = AddSignedDecimal(left.Text, right.Text)
	case "-":
		out, ok = SubtractSignedDecimal(left.Text, right.Text)
	case "*":
		out, ok = MultiplySignedDecimal(left.Text, right.Text)
	case "/":
		out, _, ok = DivideSignedDecimal(left.Text, right.Text)
	case "%":
		_, out, ok = DivideSignedDecimal(left.Text, right.Text)
	case "&", "|", "^", "&^":
		out, ok = BitwiseSignedDecimal(operator, left.Text, right.Text)
	case "<<", ">>":
		shift, valid := shiftCount(right.Text)
		if !valid {
			return Value{}, false
		}
		factor := Pow2UnsignedDecimal(int(shift))
		if operator == "<<" {
			out, ok = MultiplySignedDecimal(left.Text, factor)
		} else {
			var remainder string
			out, remainder, ok = DivideSignedDecimal(left.Text, factor)
			if ok && strings.HasPrefix(left.Text, "-") && remainder != "0" {
				out, ok = SubtractSignedDecimal(out, "1")
			}
		}
	}
	if !ok {
		return Value{}, false
	}
	left = numericFact(Rational{Numerator: out, Denominator: "1"}, left.Type, left.Untyped)
	if operator == "<<" || operator == ">>" {
		return left, true
	}
	if left.Untyped && !right.Untyped {
		left.Type = right.Type
	}
	left.Untyped = left.Untyped && right.Untyped
	return left, true
}

// Int64 returns the exact integer when it is representable by int64.
func (value Value) Int64() (int64, bool) {
	if value.IsString {
		return 0, false
	}
	rational, ok := value.Rational()
	if !ok {
		return 0, false
	}
	integer, ok := rational.Integer()
	if !ok {
		return 0, false
	}
	return SignedDecimalInt64(integer)
}

// JSON returns the canonical artifact representation of value.
func (value Value) JSON() json.RawMessage {
	if value.IsString {
		return bytecode.EncodeStringConstant(value.Text)
	}
	if value.Text == "null" {
		return json.RawMessage("null")
	}
	if value.Imag != "" {
		raw, _ := json.Marshal(struct {
			Real string `json:"real"`
			Imag string `json:"imag"`
		}{value.Real, value.Imag})
		return raw
	}
	if value.Text == "true" || value.Text == "false" {
		return json.RawMessage(value.Text)
	}
	if strings.ContainsAny(value.Text, ".eE") || value.Text == "-0" {
		return json.RawMessage(value.Text)
	}
	if rational, ok := value.Rational(); ok {
		if integer, integerOK := rational.Integer(); integerOK {
			if _, int64OK := SignedDecimalInt64(integer); int64OK {
				return json.RawMessage(integer)
			}
		}
	}
	raw, _ := json.Marshal(value.Text)
	return raw
}

// Encode applies the artifact's numeric representation at the emission boundary.
// The type and untyped flag belong to the HIR operand, not the shared literal.
func (value *Value) Encode(table *types.TypeTable, ref types.TypeRef, untyped bool) json.RawMessage {
	if value == nil {
		return nil
	}
	underlying := table.Underlying(ref)
	if underlying.Kind == types.Slice && value.IsString {
		if elem, ok := types.View(table, underlying).Elem(); ok && table.Underlying(elem) == types.Builtin(types.PrimitiveUint8) {
			return bytecode.EncodeStringConstant(base64.StdEncoding.EncodeToString([]byte(value.Text)))
		}
	}
	primitive := underlying.Primitive
	if untyped && (primitive == types.PrimitiveFloat32 || primitive == types.PrimitiveFloat64) {
		if number, ok := value.Rational(); ok {
			data, _ := json.Marshal(number.Numerator + "/" + number.Denominator)
			return data
		}
	}
	if untyped && (primitive == types.PrimitiveComplex64 || primitive == types.PrimitiveComplex128) {
		r, rok := ParseRationalLiteral(value.Real)
		i, iok := ParseRationalLiteral(value.Imag)
		if rok && iok {
			data, _ := json.Marshal(struct {
				Real string `json:"real"`
				Imag string `json:"imag"`
			}{r.Numerator + "/" + r.Denominator, i.Numerator + "/" + i.Denominator})
			return data
		}
	}
	if !untyped && (primitive == types.PrimitiveFloat32 || primitive == types.PrimitiveFloat64) {
		if value.Text == "-0" {
			return json.RawMessage("-0")
		}
		number, ok := value.Rational()
		bits := 64
		if primitive == types.PrimitiveFloat32 {
			bits = 32
		}
		if ok {
			converted, valid := RationalFloat(number, bits)
			if valid {
				return json.RawMessage(strconv.FormatFloat(converted, 'g', -1, 64))
			}
		}
	}
	if !untyped && (primitive == types.PrimitiveComplex64 || primitive == types.PrimitiveComplex128) {
		realPart, realOK := ParseRationalLiteral(value.Real)
		imagPart, imagOK := ParseRationalLiteral(value.Imag)
		bits := 64
		if primitive == types.PrimitiveComplex64 {
			bits = 32
		}
		r, rok := RationalFloat(realPart, bits)
		i, iok := RationalFloat(imagPart, bits)
		if realOK && imagOK && rok && iok {
			data, _ := json.Marshal(struct {
				Real float64 `json:"real"`
				Imag float64 `json:"imag"`
			}{r, i})
			return data
		}
	}
	if (primitive >= types.PrimitiveUint && primitive <= types.PrimitiveUintptr) && value.Kind() == Number {
		data, _ := json.Marshal(value.Text)
		return data
	}
	return value.JSON()
}

func integerType(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "Int", "Int8", "Int16", "Int32", "Int64", "Uint", "Uint8", "Uint16", "Uint32", "Uint64", "Uintptr", "Byte", "Rune":
		return true
	default:
		return false
	}
}

func shiftCount(text string) (uint, bool) {
	text, ok := NormalizeSignedDecimal(text)
	if !ok || strings.HasPrefix(text, "-") {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value > 4096 {
		return 0, false
	}
	return uint(value), true
}

func integerDigit(digit rune) int {
	switch {
	case digit >= '0' && digit <= '9':
		return int(digit - '0')
	case digit >= 'a' && digit <= 'f':
		return int(digit-'a') + 10
	case digit >= 'A' && digit <= 'F':
		return int(digit-'A') + 10
	default:
		return -1
	}
}
