package constant

import (
	"strconv"
	"strings"
)

const maxLiteralExponent = 100000

// Rational is a canonical arbitrary-precision source constant.
type Rational struct {
	Numerator   string
	Denominator string
}

// NewRational canonicalizes numerator/denominator and removes their common
// divisor.
func NewRational(numerator, denominator string) (Rational, bool) {
	numerator, numeratorOK := NormalizeSignedDecimal(numerator)
	denominator, denominatorOK := NormalizeSignedDecimal(denominator)
	if !numeratorOK || !denominatorOK || denominator == "0" {
		return Rational{}, false
	}
	if strings.HasPrefix(denominator, "-") {
		denominator = strings.TrimPrefix(denominator, "-")
		numerator, _ = NegateSignedDecimal(numerator)
	}
	if numerator == "0" {
		return Rational{Numerator: "0", Denominator: "1"}, true
	}
	if denominator == "1" {
		return Rational{Numerator: numerator, Denominator: denominator}, true
	}
	absNumerator := strings.TrimPrefix(numerator, "-")
	divisor := gcdCanonicalDecimal(absNumerator, denominator)
	if divisor != "1" {
		absNumerator, _ = divideCanonicalDecimal(absNumerator, divisor)
		denominator, _ = divideCanonicalDecimal(denominator, divisor)
		if strings.HasPrefix(numerator, "-") {
			absNumerator = "-" + absNumerator
		}
		numerator = absNumerator
	}
	return Rational{Numerator: numerator, Denominator: denominator}, true
}

// Valid reports whether r is initialized and canonical.
func (r Rational) Valid() bool { return r.Denominator != "" }

// IsZero reports whether r is the exact value zero.
func (r Rational) IsZero() bool { return r.Valid() && r.Numerator == "0" }

// Integer returns the numerator when r has no fractional component.
func (r Rational) Integer() (string, bool) {
	return r.Numerator, r.Valid() && r.Denominator == "1"
}

// String returns the canonical integer or numerator/denominator form.
func (r Rational) String() string {
	if !r.Valid() {
		return ""
	}
	if r.Denominator == "1" {
		return r.Numerator
	}
	return r.Numerator + "/" + r.Denominator
}

// NegateRational returns -value.
func NegateRational(value Rational) (Rational, bool) {
	numerator, ok := NegateSignedDecimal(value.Numerator)
	return Rational{Numerator: numerator, Denominator: value.Denominator}, ok
}

// AddRational returns left+right.
func AddRational(left, right Rational) (Rational, bool) {
	leftNumerator, ok := MultiplySignedDecimal(left.Numerator, right.Denominator)
	if !ok {
		return Rational{}, false
	}
	rightNumerator, ok := MultiplySignedDecimal(right.Numerator, left.Denominator)
	if !ok {
		return Rational{}, false
	}
	numerator, ok := AddSignedDecimal(leftNumerator, rightNumerator)
	if !ok {
		return Rational{}, false
	}
	denominator, ok := MultiplySignedDecimal(left.Denominator, right.Denominator)
	if !ok {
		return Rational{}, false
	}
	return NewRational(numerator, denominator)
}

// SubtractRational returns left-right.
func SubtractRational(left, right Rational) (Rational, bool) {
	negated, ok := NegateRational(right)
	if !ok {
		return Rational{}, false
	}
	return AddRational(left, negated)
}

// MultiplyRational returns left*right.
func MultiplyRational(left, right Rational) (Rational, bool) {
	numerator, ok := MultiplySignedDecimal(left.Numerator, right.Numerator)
	if !ok {
		return Rational{}, false
	}
	denominator, ok := MultiplySignedDecimal(left.Denominator, right.Denominator)
	if !ok {
		return Rational{}, false
	}
	return NewRational(numerator, denominator)
}

// DivideRational returns left/right and rejects division by zero.
func DivideRational(left, right Rational) (Rational, bool) {
	if right.IsZero() {
		return Rational{}, false
	}
	numerator, ok := MultiplySignedDecimal(left.Numerator, right.Denominator)
	if !ok {
		return Rational{}, false
	}
	denominator, ok := MultiplySignedDecimal(left.Denominator, right.Numerator)
	if !ok {
		return Rational{}, false
	}
	return NewRational(numerator, denominator)
}

// CompareRational compares left and right and returns -1, 0, or 1.
func CompareRational(left, right Rational) int {
	leftScaled, leftOK := MultiplySignedDecimal(left.Numerator, right.Denominator)
	rightScaled, rightOK := MultiplySignedDecimal(right.Numerator, left.Denominator)
	if !leftOK || !rightOK {
		return 0
	}
	return CompareSignedDecimal(leftScaled, rightScaled)
}

// ParseRationalLiteral parses an integer, decimal, hexadecimal floating-point,
// or canonical numerator/denominator source constant.
func ParseRationalLiteral(text string) (Rational, bool) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "_", ""))
	if text == "" || strings.HasSuffix(text, "i") {
		return Rational{}, false
	}
	if slash := strings.IndexByte(text, '/'); slash >= 0 {
		if strings.IndexByte(text[slash+1:], '/') >= 0 {
			return Rational{}, false
		}
		return NewRational(text[:slash], text[slash+1:])
	}
	if strings.HasPrefix(text, "0x") || strings.HasPrefix(text, "0X") || strings.ContainsAny(text, "pP") {
		return parseHexRational(text)
	}
	if !strings.ContainsAny(text, ".eE") {
		value, ok := ParseIntegerLiteral(text)
		if !ok {
			return Rational{}, false
		}
		return NewRational(value.Text, "1")
	}
	return parseDecimalRational(text)
}

func parseDecimalRational(text string) (Rational, bool) {
	sign := ""
	if text != "" && (text[0] == '+' || text[0] == '-') {
		if text[0] == '-' {
			sign = "-"
		}
		text = text[1:]
	}
	exponent := 0
	if at := strings.IndexAny(text, "eE"); at >= 0 {
		parsed, ok := parseLiteralExponent(text[at+1:])
		if !ok {
			return Rational{}, false
		}
		exponent, text = parsed, text[:at]
	}
	fractionDigits := 0
	if at := strings.IndexByte(text, '.'); at >= 0 {
		if strings.IndexByte(text[at+1:], '.') >= 0 {
			return Rational{}, false
		}
		fractionDigits = len(text) - at - 1
		text = text[:at] + text[at+1:]
	}
	if text == "" {
		return Rational{}, false
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return Rational{}, false
		}
	}
	coefficient, ok := NormalizeSignedDecimal(sign + text)
	if !ok {
		return Rational{}, false
	}
	power := exponent - fractionDigits
	if power >= 0 {
		return NewRational(coefficient+strings.Repeat("0", power), "1")
	}
	return NewRational(coefficient, "1"+strings.Repeat("0", -power))
}

func parseHexRational(text string) (Rational, bool) {
	sign := ""
	if text != "" && (text[0] == '+' || text[0] == '-') {
		if text[0] == '-' {
			sign = "-"
		}
		text = text[1:]
	}
	if len(text) < 3 || text[0] != '0' || text[1] != 'x' && text[1] != 'X' {
		return Rational{}, false
	}
	exponentAt := strings.IndexAny(text, "pP")
	if exponentAt < 0 {
		if strings.Contains(text, ".") {
			return Rational{}, false
		}
		value, ok := ParseIntegerLiteral(sign + text)
		if !ok {
			return Rational{}, false
		}
		return NewRational(value.Text, "1")
	}
	exponent, ok := parseLiteralExponent(text[exponentAt+1:])
	if !ok {
		return Rational{}, false
	}
	mantissa := text[2:exponentAt]
	fractionDigits := 0
	if at := strings.IndexByte(mantissa, '.'); at >= 0 {
		if strings.IndexByte(mantissa[at+1:], '.') >= 0 {
			return Rational{}, false
		}
		fractionDigits = len(mantissa) - at - 1
		mantissa = mantissa[:at] + mantissa[at+1:]
	}
	if mantissa == "" {
		return Rational{}, false
	}
	coefficient := "0"
	for _, digit := range mantissa {
		n := numericDigit(digit)
		if n < 0 || n >= 16 {
			return Rational{}, false
		}
		coefficient, _ = MultiplySignedDecimal(coefficient, "16")
		coefficient, _ = AddSignedDecimal(coefficient, strconv.Itoa(n))
	}
	coefficient, _ = NormalizeSignedDecimal(sign + coefficient)
	power := exponent - fractionDigits*4
	if power >= 0 {
		coefficient, ok = MultiplySignedDecimal(coefficient, Pow2UnsignedDecimal(power))
		if !ok {
			return Rational{}, false
		}
		return NewRational(coefficient, "1")
	}
	return NewRational(coefficient, Pow2UnsignedDecimal(-power))
}

func parseLiteralExponent(text string) (int, bool) {
	if text == "" {
		return 0, false
	}
	negative := false
	if text[0] == '+' || text[0] == '-' {
		negative, text = text[0] == '-', text[1:]
	}
	if text == "" {
		return 0, false
	}
	value := 0
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		value = value*10 + int(digit-'0')
		if value > maxLiteralExponent {
			return 0, false
		}
	}
	if negative {
		value = -value
	}
	return value, true
}

func numericDigit(digit rune) int {
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
