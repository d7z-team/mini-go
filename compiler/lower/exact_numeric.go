package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/constant"
)

type exactRational = constant.Rational

type exactComplex struct {
	realPart      exactRational
	imaginaryPart exactRational
}

func foldExactRationalBinary(operator string, left, right exactRational) (exactRational, bool) {
	switch operator {
	case "+":
		return constant.AddRational(left, right)
	case "-":
		return constant.SubtractRational(left, right)
	case "*":
		return constant.MultiplyRational(left, right)
	case "/":
		return constant.DivideRational(left, right)
	default:
		return exactRational{}, false
	}
}

func foldExactRationalCompare(operator string, left, right exactRational) (*constant.Value, bool) {
	comparison := constant.CompareRational(left, right)
	switch operator {
	case "==":
		return booleanConstant(comparison == 0), true
	case "!=":
		return booleanConstant(comparison != 0), true
	case "<":
		return booleanConstant(comparison < 0), true
	case "<=":
		return booleanConstant(comparison <= 0), true
	case ">":
		return booleanConstant(comparison > 0), true
	case ">=":
		return booleanConstant(comparison >= 0), true
	default:
		return nil, false
	}
}

func foldExactComplexBinary(operator string, left, right exactComplex) (exactComplex, bool) {
	value, ok := constant.Binary(operator,
		constant.Value{Real: left.realPart.String(), Imag: left.imaginaryPart.String(), Type: "Complex128"},
		constant.Value{Real: right.realPart.String(), Imag: right.imaginaryPart.String(), Type: "Complex128"})
	if !ok {
		return exactComplex{}, false
	}
	realPart, realOK := constant.ParseRationalLiteral(value.Real)
	imaginaryPart, imagOK := constant.ParseRationalLiteral(value.Imag)
	return exactComplex{realPart: realPart, imaginaryPart: imaginaryPart}, realOK && imagOK
}

func parseExactIntegerLiteral(text string) (string, bool) {
	value, ok := constant.ParseIntegerLiteral(text)
	return value.Text, ok
}

func parseExactRationalLiteral(text string) (exactRational, bool) {
	return constant.ParseRationalLiteral(text)
}

func parseExactImaginaryLiteral(text string) (exactComplex, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasSuffix(text, "i") {
		return exactComplex{}, false
	}
	imagPart, ok := parseExactRationalLiteral(strings.TrimSuffix(text, "i"))
	if !ok {
		return exactComplex{}, false
	}
	zero, _ := constant.NewRational("0", "1")
	return exactComplex{realPart: zero, imaginaryPart: imagPart}, true
}

func exactRationalConstant(number exactRational) (*constant.Value, bool) {
	value, ok := constant.Numeric(number.String(), "Float64", true)
	return value.Ref(), ok
}

func exactIntegerConstant(text string) (*constant.Value, bool) {
	value, ok := constant.Integer(text, "Int", true)
	return value.Ref(), ok
}

func exactComplexConstant(value exactComplex) (*constant.Value, bool) {
	return &constant.Value{Real: value.realPart.String(), Imag: value.imaginaryPart.String(), Type: "Complex128", Untyped: true}, value.realPart.Valid() && value.imaginaryPart.Valid()
}

func constantRational(value *constant.Value) (exactRational, bool) {
	if value == nil {
		return exactRational{}, false
	}
	return value.Rational()
}

func constantNegativeZero(value *constant.Value) bool {
	if value == nil || !strings.HasPrefix(value.Text, "-") {
		return false
	}
	number, ok := value.Rational()
	return ok && number.IsZero()
}

func constantComplex(value *constant.Value) (exactComplex, bool) {
	if value == nil || value.Kind() != constant.ComplexValue {
		return exactComplex{}, false
	}
	realPart, realOK := constant.ParseRationalLiteral(value.Real)
	imagPart, imagOK := constant.ParseRationalLiteral(value.Imag)
	return exactComplex{realPart: realPart, imaginaryPart: imagPart}, realOK && imagOK
}

func (l *lowerer) constExactRational(raw *constant.Value, typ string) (exactRational, bool) {
	kind := l.underlyingConstType(typ)
	if !isIntegerType(kind) && !isFloatType(kind) {
		return exactRational{}, false
	}
	return constantRational(raw)
}

func (l *lowerer) constExactComplex(raw *constant.Value, typ string) (exactComplex, bool) {
	kind := l.underlyingConstType(typ)
	if isComplexType(kind) {
		return constantComplex(raw)
	}
	if isIntegerType(kind) || isFloatType(kind) {
		realPart, ok := constantRational(raw)
		if !ok {
			return exactComplex{}, false
		}
		zero, _ := constant.NewRational("0", "1")
		return exactComplex{realPart: realPart, imaginaryPart: zero}, true
	}
	return exactComplex{}, false
}

func (l *lowerer) rationalConstantForType(value exactRational, typ string, untyped bool) (*constant.Value, bool) {
	if untyped {
		return exactRationalConstant(value)
	}
	kind := l.underlyingConstType(typ)
	bits := 64
	if kind == "Float32" {
		bits = 32
	}
	converted, ok := constant.RationalFloat(value, bits)
	if !ok {
		return nil, false
	}
	return floatConstant(converted)
}

func (l *lowerer) complexConstantForType(value exactComplex, typ string, untyped bool) (*constant.Value, bool) {
	if untyped {
		return exactComplexConstant(value)
	}
	kind := l.underlyingConstType(typ)
	bits := 64
	if kind == "Complex128" {
		bits = 128
	}
	componentBits := bits / 2
	realPart, realOK := constant.RationalFloat(value.realPart, componentBits)
	imagPart, imagOK := constant.RationalFloat(value.imaginaryPart, componentBits)
	if !realOK || !imagOK {
		return nil, false
	}
	return complexConstant(complex(realPart, imagPart))
}
