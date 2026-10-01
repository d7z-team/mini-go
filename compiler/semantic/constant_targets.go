package semantic

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/types"
)

// validateConstantTarget handles contextual typing without changing the source
// expression's exact value. It reports whether ordinary assignability is handled.
func (a *analyzer) validateConstantTarget(expr ast.Expression, target types.TypeRef, conversion bool) bool {
	info := a.info.Exprs[expr.NodeID]
	if !target.Valid() || !info.Untyped && info.Mode != ExprConstant {
		return false
	}
	view := a.info.Relations.View(target)
	if target.Kind == types.TypeParameter {
		terms, ok := a.typeParameterTerms(target)
		if ok {
			for _, term := range terms {
				a.validateConstantTarget(expr, term, conversion)
			}
		}
		return true
	}
	if !a.info.TypeExact(target) {
		return true
	}
	if expr.Name == "nil" || expr.Literal == "nil" {
		switch view.Shape() {
		case types.Pointer, types.Slice, types.Map, types.Function, types.Interface, types.Any, types.Waitable:
			return true
		}
		a.addDiagnostic("semantic.assign.type", "nil is not assignable to "+types.FormatWithTable(a.info.TypeTable, target), expr.Span)
		return true
	}
	if target.Kind == types.Any || view.Shape() == types.Interface {
		if info.Untyped {
			return a.validateConstantTarget(expr, info.Type, false)
		}
		return false
	}
	if !info.Untyped && !conversion {
		if !a.info.Relations.Assignable(info.Type, target).OK {
			return false
		}
	}
	sourceView := a.info.Relations.View(info.Type)
	primitive, _ := view.Primitive()
	sourcePrimitive, _ := sourceView.Primitive()
	if conversion && sourcePrimitive == types.PrimitiveString && view.Shape() == types.Slice {
		if !a.typesConvertible(info.Type, target) {
			a.addDiagnostic("semantic.convert.type", "cannot convert string to "+types.FormatWithTable(a.info.TypeTable, target), expr.Span)
		}
		return true
	}
	var valid bool
	if primitive == types.PrimitiveBool || primitive == types.PrimitiveString {
		valid = primitive == sourcePrimitive
		if !valid && conversion && primitive == types.PrimitiveString {
			value, ok := a.evaluateConstantExpression(expr, a.info.NodeScopes[expr.NodeID])
			rational, numeric := value.Rational()
			_, integer := rational.Integer()
			valid = ok && numeric && integer
		}
	} else if _, ok := view.NumericInfo(); ok {
		_, sourceNumeric := sourceView.NumericInfo()
		valid = sourceNumeric
		if value, ok := a.evaluateConstantExpression(expr, a.info.NodeScopes[expr.NodeID]); ok && valid {
			valid = a.constantValueFits(value, target, conversion)
			if !value.Untyped && !a.constantValueFits(value, info.Type, true) {
				valid = false
			}
		}
	} else {
		valid = a.info.Relations.Assignable(info.Type, target).OK
	}
	if !valid {
		code := "semantic.const.representable"
		if _, numeric := view.NumericInfo(); !numeric {
			code = "semantic.assign.type"
			if conversion {
				code = "semantic.convert.type"
			}
		}
		a.addDiagnostic(code, "constant is not representable by "+types.FormatWithTable(a.info.TypeTable, target), expr.Span)
	}
	return true
}

func (a *analyzer) constantValueFits(value constant.Value, target types.TypeRef, conversion bool) bool {
	source, err := a.parser.Parse(value.Type)
	if err != nil {
		return false
	}
	if !value.Untyped && !conversion && !a.info.Relations.Assignable(source, target).OK {
		return false
	}
	view := a.info.Relations.View(target)
	if numeric, ok := view.NumericInfo(); ok {
		var rational constant.Rational
		var ok bool
		if value.Imag != "" {
			imaginaryPart, valid := constant.ParseRationalLiteral(value.Imag)
			if !valid {
				return false
			}
			if numeric.Kind == types.NumericComplex {
				if !constant.FloatRepresentable(imaginaryPart, numeric.Bits/2) {
					return false
				}
			} else if imaginaryPart.Numerator != "0" {
				return false
			}
			rational, ok = constant.ParseRationalLiteral(value.Real)
		} else {
			rational, ok = value.Rational()
		}
		if !ok {
			return false
		}
		switch numeric.Kind {
		case types.NumericSigned, types.NumericUnsigned:
			return constant.IntegerRepresentable(rational, numeric.Bits, numeric.Kind == types.NumericSigned)
		case types.NumericFloat:
			return constant.FloatRepresentable(rational, numeric.Bits)
		case types.NumericComplex:
			return constant.FloatRepresentable(rational, numeric.Bits/2)
		}
	}
	if value.Untyped {
		sourcePrimitive, _ := a.info.Relations.View(source).Primitive()
		targetPrimitive, _ := view.Primitive()
		if sourcePrimitive == targetPrimitive && (sourcePrimitive == types.PrimitiveString || sourcePrimitive == types.PrimitiveBool) {
			return true
		}
	}
	return a.info.Relations.Assignable(source, target).OK
}
