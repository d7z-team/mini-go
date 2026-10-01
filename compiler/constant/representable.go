package constant

import "strings"

// IntegerRepresentable reports whether an exact rational fits an integer target.
func IntegerRepresentable(value Rational, bits int, signed bool) bool {
	integer, ok := value.Integer()
	if !ok || bits < 1 {
		return false
	}
	if signed {
		lower, upper := SignedIntegerBounds(bits)
		return CompareSignedDecimal(integer, lower) >= 0 && CompareSignedDecimal(integer, upper) <= 0
	}
	upper := UnsignedIntegerMax(bits)
	return !strings.HasPrefix(integer, "-") && CompareUnsignedDecimal(integer, upper) <= 0
}

// FloatRepresentable checks finite IEEE round-to-nearest representation. The
// midpoint above the maximum finite number rounds to infinity and is excluded.
func FloatRepresentable(value Rational, bits int) bool {
	if !value.Valid() || bits != 32 && bits != 64 {
		return false
	}
	// Exact overflow midpoints: 2^128 - 2^103 and 2^1024 - 2^970.
	// These are properties of the two target formats, not per-value work.
	cutoff := "179769313486231580793728971405303415079934132710037826936173778980444968292764750946649017977587207096330286416692887910946555547851940402630657488671505820681908902000708383676273854845817711531764475730270069855571366959622842914819860834936475292719074168444365510704342711559699508093042880177904174497792"
	if bits == 32 {
		cutoff = "340282356779733661637539395458142568448"
	}
	value.Numerator = strings.TrimPrefix(value.Numerator, "-")
	// A canonical rational's positive denominator is at least one.
	if len(value.Numerator) < len(cutoff) {
		return true
	}
	return CompareRational(value, Rational{Numerator: cutoff, Denominator: "1"}) < 0
}

func SignedIntegerBounds(bits int) (string, string) {
	switch bits {
	case 8:
		return "-128", "127"
	case 16:
		return "-32768", "32767"
	case 32:
		return "-2147483648", "2147483647"
	case 64:
		return "-9223372036854775808", "9223372036854775807"
	}
	if bits <= 0 {
		return "0", "0"
	}
	maxBase := Pow2UnsignedDecimal(bits - 1)
	upper, _ := SubtractUnsignedDecimal(maxBase, "1")
	return "-" + maxBase, upper
}

func UnsignedIntegerMax(bits int) string {
	switch bits {
	case 8:
		return "255"
	case 16:
		return "65535"
	case 32:
		return "4294967295"
	case 64:
		return "18446744073709551615"
	}
	if bits <= 0 {
		return "0"
	}
	upper, _ := SubtractUnsignedDecimal(Pow2UnsignedDecimal(bits), "1")
	return upper
}
