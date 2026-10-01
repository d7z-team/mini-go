package constant

import "testing"

func TestRationalIntegerNormalizationPreservesSignsAndPrecision(t *testing.T) {
	for _, test := range []struct{ numerator, denominator, want string }{
		{"+00042", "+0001", "42"},
		{"-00042", "-0001", "42"},
		{"42", "-1", "-42"},
		{"-000", "-1", "0"},
		{"18446744073709551616", "1", "18446744073709551616"},
		{"-340282366920938463463374607431768211455", "-1", "340282366920938463463374607431768211455"},
	} {
		got, ok := NewRational(test.numerator, test.denominator)
		if !ok || got.Numerator != test.want || got.Denominator != "1" {
			t.Fatalf("%s / %s = %+v, %v; want %s", test.numerator, test.denominator, got, ok, test.want)
		}
	}
	for _, numerator := range []string{"", "+", "12x", "1.5"} {
		if got, ok := NewRational(numerator, "1"); ok {
			t.Fatalf("invalid numerator %q accepted: %+v", numerator, got)
		}
	}
}

func TestRationalLiteralCanonicalization(t *testing.T) {
	tests := map[string]string{
		"1.25":      "5/4",
		"1e3":       "1000",
		"0x1p-3":    "1/8",
		"0x1p-149":  "1/713623846352979940529142984724747568191373312",
		"-12.50e-1": "-5/4",
	}
	for literal, want := range tests {
		value, ok := ParseRationalLiteral(literal)
		if !ok || value.String() != want {
			t.Fatalf("ParseRationalLiteral(%q) = %q, %v, want %q", literal, value.String(), ok, want)
		}
	}
	left, _ := Numeric("1", "Int", true)
	right, _ := Numeric("0.25", "Float64", true)
	value, ok := Binary("/", left, right)
	if !ok || value.Text != "4" || value.Type != "Float64" {
		t.Fatalf("exact division = %#v, %v", value, ok)
	}
}

func TestRationalArithmeticAndInvalidInputs(t *testing.T) {
	one, _ := NewRational("1", "1")
	three, _ := NewRational("3", "1")
	third, ok := DivideRational(one, three)
	if !ok || third.String() != "1/3" {
		t.Fatalf("1/3 = %q, %v", third.String(), ok)
	}
	whole, ok := MultiplyRational(third, three)
	if !ok || whole.String() != "1" || CompareRational(whole, one) != 0 {
		t.Fatalf("(1/3)*3 = %q, %v", whole.String(), ok)
	}
	zero, _ := NewRational("0", "1")
	if _, ok := DivideRational(one, zero); ok {
		t.Fatal("division by zero succeeded")
	}
	for _, text := range []string{"", "1/0", "1/2/3", "0x.p1", "1e100001"} {
		if _, ok := ParseRationalLiteral(text); ok {
			t.Fatalf("ParseRationalLiteral(%q) succeeded", text)
		}
	}
}
