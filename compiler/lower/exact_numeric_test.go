package lower

import (
	"math"
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
)

func TestExactNumericParsesAndFoldsWithoutHostPrecisionLoss(t *testing.T) {
	one, _ := parseExactRationalLiteral("1.0")
	three, _ := parseExactRationalLiteral("3.0")
	third, ok := foldExactRationalBinary("/", one, three)
	if !ok {
		t.Fatal("exact division failed")
	}
	whole, ok := foldExactRationalBinary("*", third, three)
	if !ok || whole.Numerator+"/"+whole.Denominator != "1/1" {
		t.Fatalf("(1/3)*3 = %q", whole.Numerator+"/"+whole.Denominator)
	}

	high, _ := parseExactRationalLiteral("9007199254740993.0")
	prior, _ := parseExactRationalLiteral("9007199254740992.0")
	comparison, ok := foldExactRationalCompare(">", high, prior)
	if !ok || string(comparison.JSON()) != "true" {
		t.Fatal("exact constants above 2^53 compared equal")
	}

	hex, ok := parseExactRationalLiteral("0x1.fp2")
	if !ok || hex.Numerator+"/"+hex.Denominator != "31/4" {
		t.Fatalf("hex float = %q", hex.Numerator+"/"+hex.Denominator)
	}
}

func TestIntegerBoundsAreCanonical(t *testing.T) {
	for _, test := range []struct {
		bits         int
		lower, upper string
		unsigned     string
	}{
		{8, "-128", "127", "255"},
		{16, "-32768", "32767", "65535"},
		{32, "-2147483648", "2147483647", "4294967295"},
		{64, "-9223372036854775808", "9223372036854775807", "18446744073709551615"},
	} {
		lower, upper := constant.SignedIntegerBounds(test.bits)
		if lower != test.lower || upper != test.upper || constant.UnsignedIntegerMax(test.bits) != test.unsigned {
			t.Fatalf("%d-bit bounds = %s..%s unsigned %s", test.bits, lower, upper, constant.UnsignedIntegerMax(test.bits))
		}
	}
}

func TestExactNumericHandlesSignedShift(t *testing.T) {
	if got, ok := foldExactIntegerBinary(">>", "-5", "1"); !ok || got != "-3" {
		t.Fatalf("-5 >> 1 = %s", got)
	}
	if got, ok := foldExactIntegerBinary("<<", "-5", "100"); !ok || got != "-6338253001141147007483516026880" {
		t.Fatalf("-5 << 100 = %s", got)
	}
}

func TestExactRationalRoundsAtIEEEBoundary(t *testing.T) {
	value, _ := parseExactRationalLiteral("9007199254740993.0")
	got, ok := constant.RationalFloat(value, 64)
	if !ok || got != 9007199254740992 {
		t.Fatalf("Float64 rounding = %.0f", got)
	}
	value, _ = parseExactRationalLiteral("0x1.000001p0")
	got, ok = constant.RationalFloat(value, 32)
	if !ok || math.Float32bits(float32(got)) != math.Float32bits(1) {
		t.Fatalf("Float32 tie-to-even bits = %#x", math.Float32bits(float32(got)))
	}
	value, _ = parseExactRationalLiteral("1e1000")
	if _, ok := constant.RationalFloat(value, 64); ok {
		t.Fatal("Float64 overflow was accepted")
	}

	for _, test := range []struct {
		literal string
		bits    int
		want    float64
	}{
		{literal: "0x1.fffffffffffffp1023", bits: 64, want: math.MaxFloat64},
		{literal: "0x1p-1074", bits: 64, want: math.SmallestNonzeroFloat64},
		{literal: "0x1p-149", bits: 32, want: float64(math.SmallestNonzeroFloat32)},
		{literal: "0x1.fffffep127", bits: 32, want: float64(math.MaxFloat32)},
	} {
		value, ok := parseExactRationalLiteral(test.literal)
		if !ok {
			t.Fatalf("parse %s failed", test.literal)
		}
		got, ok := constant.RationalFloat(value, test.bits)
		if !ok || got != test.want {
			t.Fatalf("%s as Float%d = %g, want %g", test.literal, test.bits, got, test.want)
		}
	}

	negativeTiny, _ := parseExactRationalLiteral("-1e-1000")
	negativeZero, ok := constant.RationalFloat(negativeTiny, 64)
	if !ok || negativeZero != 0 || !math.Signbit(negativeZero) {
		t.Fatalf("negative underflow = %g signbit=%v", negativeZero, math.Signbit(negativeZero))
	}
}
