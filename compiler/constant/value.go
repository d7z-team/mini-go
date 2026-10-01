// Package constant implements exact compile-time Mini-Go values and arithmetic.
package constant

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/runtime/bytecode"
)

// Value is a source-level exact fact. Text is a canonical arbitrary precision
// number or Go-quoted string and Type is the source type identity.
type Value struct {
	Text            string
	Type            string
	Untyped         bool
	Real            string
	Imag            string
	canonicalNumber string
	numeratorEnd    int
}

// Rational returns the exact numeric value. Constructor-owned facts are tied
// to their immutable text; edited or decoded values are parsed normally.
func (value Value) Rational() (Rational, bool) {
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
)

// Kind distinguishes exact values independently of their declared type identity.
func (value Value) Kind() Kind {
	if value.Imag != "" {
		return ComplexValue
	}
	if value.Text == "true" || value.Text == "false" {
		return Boolean
	}
	if strings.HasPrefix(value.Text, "\"") {
		return StringValue
	}
	if value.Text != "" {
		return Number
	}
	return Invalid
}

// String constructs an exact string value using Go quoting.
func String(text, typ string, untyped bool) Value {
	return Value{Text: strconv.Quote(text), Type: typ, Untyped: untyped}
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
			Real string `json:"real"`
			Imag string `json:"imag"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return Value{}, false
		}
		realPart, realOK := ParseRationalLiteral(parts.Real)
		imaginaryPart, imagOK := ParseRationalLiteral(parts.Imag)
		return Value{Real: realPart.String(), Imag: imaginaryPart.String(), Type: typ, Untyped: untyped}, realOK && imagOK
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
	if err := json.Unmarshal(raw, &text); err != nil {
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
			l, le := strconv.Unquote(left.Text)
			r, re := strconv.Unquote(right.Text)
			if le != nil || re != nil {
				return Value{}, false
			}
			comparison = strings.Compare(l, r)
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
	if operator == "+" && strings.HasPrefix(left.Text, "\"") && strings.HasPrefix(right.Text, "\"") {
		leftText, leftErr := strconv.Unquote(left.Text)
		rightText, rightErr := strconv.Unquote(right.Text)
		if leftErr != nil || rightErr != nil {
			return Value{}, false
		}
		if left.Untyped && !right.Untyped {
			left.Type = right.Type
		}
		return String(leftText+rightText, left.Type, left.Untyped && right.Untyped), true
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
	if text, err := strconv.Unquote(value.Text); err == nil {
		return bytecode.EncodeStringConstant(text)
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
