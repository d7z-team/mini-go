package constant

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

func TestIntegerLiteralParsingPreservesBasesSignsAndArbitraryPrecision(t *testing.T) {
	for _, input := range []string{
		"0", "-0", "+0", "000", "-000", "42", "-42", "+42", "0_7_7",
		"0xff", "-0XFF", "0b101010", "0o52", "077", "18446744073709551615",
		"18446744073709551616", "0x10000000000000000", "-0xffffffffffffffffffffffff",
		strings.Repeat("9", 1024),
	} {
		want, ok := new(big.Int).SetString(input, 0)
		if !ok {
			t.Fatalf("invalid test literal %q", input)
		}
		got, ok := ParseIntegerLiteral(input)
		if !ok || got.Text != want.String() || got.Type != "Int" || !got.Untyped {
			t.Fatalf("ParseIntegerLiteral(%q) = %#v, %v; want %s", input, got, ok, want)
		}
		if number, valid := got.Rational(); !valid || number.Numerator != want.String() || number.Denominator != "1" {
			t.Fatalf("integer fact for %q = %+v, %v", input, number, valid)
		}
	}
	for _, input := range []string{"", "+", "-", "0x", "0b2", "08", "1e3", "1.2", "+ 42", "0x+1", "0x-1", "12 3", "４２", "1\x002"} {
		if got, ok := ParseIntegerLiteral(input); ok {
			t.Fatalf("ParseIntegerLiteral(%q) = %#v; want invalid input", input, got)
		}
	}
}

func TestNumericFactsFollowTextEditsAndSerialization(t *testing.T) {
	original, ok := Numeric("1.25", "Float64", true)
	if !ok {
		t.Fatal("invalid fixture")
	}
	for _, text := range []string{"-3.5", "8/6", "0x10", "18446744073709551616", "invalid", "", original.Text} {
		value := original
		value.Text = text
		got, valid := value.Rational()
		want, wantValid := ParseRationalLiteral(text)
		if got != want || valid != wantValid {
			t.Fatalf("edited fact %q = %+v, %v; want %+v, %v", text, got, valid, want, wantValid)
		}
	}
	encoded, err := json.Marshal(original)
	if err != nil || string(encoded) != `{"Text":"5/4","Type":"Float64","Untyped":true,"Real":"","Imag":""}` {
		t.Fatalf("numeric fact wire: %s, %v", encoded, err)
	}
	for _, value := range []Value{{}, original} {
		if err := json.Unmarshal([]byte(`{"Text":"6/4","Type":"Float64","Untyped":true}`), &value); err != nil {
			t.Fatal(err)
		}
		number, valid := value.Rational()
		if !valid || number.Numerator != "3" || number.Denominator != "2" {
			t.Fatalf("decoded fact = %+v, %v", number, valid)
		}
	}
	negated, ok := Unary("-", original)
	if number, valid := negated.Rational(); !ok || !valid || number.String() != "-5/4" {
		t.Fatalf("negated fact = %+v, %v", number, valid)
	}
	added, ok := Binary("+", original, negated)
	if number, valid := added.Rational(); !ok || !valid || number.String() != "0" {
		t.Fatalf("sum fact = %+v, %v", number, valid)
	}
	if number, valid := original.Rational(); !valid || number.String() != "5/4" {
		t.Fatalf("original fact mutated: %+v, %v", number, valid)
	}
}

func BenchmarkNumericFactReuse(b *testing.B) {
	value, ok := Numeric("123456789012345678901234567890123456789/100000000000000000000000000000000000001", "Float64", true)
	if !ok {
		b.Fatal("invalid fixture")
	}
	b.Run("parse", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = ParseRationalLiteral(value.Text)
		}
	})
	b.Run("fact", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = value.Rational()
		}
	})
}

func TestRoundedNumericFactsPreserveExactArithmeticAndInputOwnership(t *testing.T) {
	value, ok := Numeric("1.1", "Float64", true)
	if !ok {
		t.Fatal("invalid fixture")
	}
	rounded, ok := value.RoundFloat(32)
	if !ok || rounded.Type != value.Type || rounded.Untyped != value.Untyped {
		t.Fatalf("rounding changed source metadata: %+v, %v", rounded, ok)
	}
	if number, valid := rounded.Rational(); !valid || number.String() != "9227469/8388608" {
		t.Fatalf("float32 rational = %+v, %v", number, valid)
	}
	difference, ok := Binary("-", rounded, value)
	if number, valid := difference.Rational(); !ok || !valid || number.String() != "1/41943040" {
		t.Fatalf("rounded difference = %+v, %v", number, valid)
	}
	rounded.Text = "-3/2"
	if number, valid := rounded.Rational(); !valid || number.String() != "-3/2" {
		t.Fatalf("edited rounded fact = %+v, %v", number, valid)
	}
	if number, valid := value.Rational(); !valid || number.String() != "11/10" {
		t.Fatalf("rounding mutated input: %+v, %v", number, valid)
	}
	for _, bits := range []int{0, 16, 128} {
		if _, valid := value.RoundFloat(bits); valid {
			t.Fatalf("unsupported precision %d accepted", bits)
		}
	}
}

func TestValueIntegerArithmetic(t *testing.T) {
	left, ok := ParseIntegerLiteral("1_000_000_000_000_000_000_000")
	if !ok {
		t.Fatal("parse left integer")
	}
	right, _ := ParseIntegerLiteral("3")
	product, ok := Binary("*", left, right)
	if !ok || product.Text != "3000000000000000000000" {
		t.Fatalf("product = %#v, %v", product, ok)
	}
	shift, _ := ParseIntegerLiteral("10")
	shifted, ok := Binary("<<", right, shift)
	if !ok || shifted.Text != "3072" {
		t.Fatalf("shift = %#v, %v", shifted, ok)
	}
}

func TestValueIntegerArithmeticPreservesNegativeRightShift(t *testing.T) {
	left, _ := ParseIntegerLiteral("-3")
	right, _ := ParseIntegerLiteral("1")
	got, ok := Binary(">>", left, right)
	if !ok || got.Text != "-2" {
		t.Fatalf("-3 >> 1 = %#v, %v", got, ok)
	}
}
