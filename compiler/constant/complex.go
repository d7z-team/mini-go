package constant

// Complex constructs an exact complex value from realPart numeric components.
func Complex(realPart, imaginaryPart Value, typ string, untyped bool) (Value, bool) {
	r, rok := realPart.Rational()
	i, iok := imaginaryPart.Rational()
	return Value{Real: r.String(), Imag: i.String(), Type: typ, Untyped: untyped}, rok && iok
}

func complexBinary(operator string, left, right Value) (Value, bool) {
	leftComplex := left.Imag != ""
	if left.Imag == "" {
		left.Real, left.Imag = left.Text, "0"
	}
	if right.Imag == "" {
		right.Real, right.Imag = right.Text, "0"
	}
	a, aok := ParseRationalLiteral(left.Real)
	b, bok := ParseRationalLiteral(left.Imag)
	c, cok := ParseRationalLiteral(right.Real)
	d, dok := ParseRationalLiteral(right.Imag)
	if !aok || !bok || !cok || !dok {
		return Value{}, false
	}
	var realPart, imaginaryPart Rational
	ok := true
	switch operator {
	case "+":
		realPart, _ = AddRational(a, c)
		imaginaryPart, _ = AddRational(b, d)
	case "-":
		realPart, _ = SubtractRational(a, c)
		imaginaryPart, _ = SubtractRational(b, d)
	case "*", "/":
		ac, _ := MultiplyRational(a, c)
		bd, _ := MultiplyRational(b, d)
		ad, _ := MultiplyRational(a, d)
		bc, _ := MultiplyRational(b, c)
		if operator == "*" {
			realPart, _ = SubtractRational(ac, bd)
			imaginaryPart, _ = AddRational(ad, bc)
		} else {
			cc, _ := MultiplyRational(c, c)
			dd, _ := MultiplyRational(d, d)
			denominator, _ := AddRational(cc, dd)
			nr, _ := AddRational(ac, bd)
			ni, _ := SubtractRational(bc, ad)
			realPart, ok = DivideRational(nr, denominator)
			if !ok {
				return Value{}, false
			}
			imaginaryPart, ok = DivideRational(ni, denominator)
		}
	default:
		return Value{}, false
	}
	typ := left.Type
	if !leftComplex || left.Untyped && !right.Untyped {
		typ = right.Type
	}
	return Value{Real: realPart.String(), Imag: imaginaryPart.String(), Type: typ, Untyped: left.Untyped && right.Untyped}, ok
}
