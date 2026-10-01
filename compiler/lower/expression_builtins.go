package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func (l *lowerer) lowerBuiltinCall(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprIdent {
		l.add("hirgen.builtin.callee", "builtin call requires identifier callee", expr.Span)
		return ir.Expression{}, false
	}
	name := expr.Callee.Name
	switch name {
	case "len", "cap":
		if len(expr.Args) != 1 {
			l.add("hirgen.builtin.arg_count", "builtin call requires exactly one argument", expr.Span)
			return ir.Expression{}, false
		}
		operandType := l.expressionType(expr.Args[0], scope)
		valid := l.isArrayType(operandType) || l.isSliceType(operandType) || l.isChannelType(operandType) || l.pointerArrayTypeOK(operandType)
		if name == "len" {
			valid = valid || l.isStringType(operandType)
		} else {
			valid = valid && !l.isStringType(operandType)
		}
		if name == "len" {
			if _, _, ok := l.mapKeyValueTypes(operandType); ok {
				valid = true
			}
		}
		if !valid {
			required := "array, slice, channel, or pointer to array"
			if name == "len" {
				required = "array, slice, string, map, channel, or pointer to array"
			}
			l.add("hirgen.builtin."+name+".type", fmt.Sprintf("%s builtin requires %s", name, required), expr.Args[0].Span)
			return ir.Expression{}, false
		}
		operand, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		if name == "cap" {
			return ir.Expression{Kind: ir.ExprCap, Operand: &operand}, true
		}
		return ir.Expression{Kind: ir.ExprLen, Operand: &operand}, true
	case "append":
		elemType, ok := l.validateAppendBuiltin(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		operand, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		var args []ir.Expression
		if expr.Ellipsis {
			if l.isStringType(l.expressionType(expr.Args[1], scope)) {
				args, ok = l.lowerExpressions(expr.Args[1:], scope)
			} else {
				args, ok = l.lowerExpressionsInType(expr.Args[1:], "Slice<"+elemType+">", scope)
			}
		} else {
			args, ok = l.lowerExpressionsInType(expr.Args[1:], elemType, scope)
		}
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprAppend, Operand: &operand, Args: args, Ellipsis: expr.Ellipsis}, true
	case "delete":
		keyType, ok := l.validateDeleteBuiltin(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		operand, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		index, ok := l.lowerExpressionInType(expr.Args[1], keyType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprDelete, Operand: &operand, Index: &index}, true
	case "clear":
		if !l.validateClearBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		operand, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprClear, Operand: &operand}, true
	case "close":
		if !l.validateCloseBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		operand, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprChanClose, Operand: &operand}, true
	case "copy":
		if !l.validateCopyBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		dst, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		src, ok := l.lowerExpression(expr.Args[1], scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprCopy, Left: &dst, Right: &src}, true
	case "min", "max":
		return l.lowerMinMaxCall(expr, scope)
	case "print", "println":
		if expr.Ellipsis {
			l.add("hirgen.builtin.print.ellipsis", name+" does not permit slice ellipsis", expr.Span)
			return ir.Expression{}, false
		}
		args, ok := l.lowerExpressionsInType(expr.Args, "Any", scope)
		if !ok {
			return ir.Expression{}, false
		}
		return l.lowerPrintBuiltinCall(name, args), true
	case "recover":
		if len(expr.Args) != 0 {
			l.add("hirgen.builtin.arg_count", "recover builtin requires no arguments", expr.Span)
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprRecover}, true
	case "complex":
		if len(expr.Args) != 2 {
			l.add("hirgen.builtin.arg_count", "complex builtin requires real and imaginary arguments", expr.Span)
			return ir.Expression{}, false
		}
		resultType := l.builtinCallType(expr, scope)
		argType := "Float64"
		if resultType == "Complex64" {
			argType = "Float32"
		}
		left, ok := l.lowerExpressionInType(expr.Args[0], argType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		right, ok := l.lowerExpressionInType(expr.Args[1], argType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprBinary, Operator: "complex", Type: l.hirType(resultType), Left: &left, Right: &right}, true
	case "real", "imag":
		if len(expr.Args) != 1 {
			l.add("hirgen.builtin.arg_count", "real and imag builtins require exactly one argument", expr.Span)
			return ir.Expression{}, false
		}
		if !l.validateComplexPartBuiltin(expr, scope) {
			return ir.Expression{}, false
		}
		operand, ok := l.lowerExpression(expr.Args[0], scope)
		if !ok {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprUnary, Operator: name, Type: l.hirType(l.builtinCallType(expr, scope)), Operand: &operand}, true
	case "new":
		if len(expr.Args) != 1 {
			l.add("hirgen.builtin.arg_count", "new builtin requires exactly one argument", expr.Span)
			return ir.Expression{}, false
		}
		return l.lowerNewBuiltin(expr, scope)
	case "make":
		if len(expr.Args) < 1 {
			l.add("hirgen.builtin.arg_count", "make builtin requires a type argument", expr.Span)
			return ir.Expression{}, false
		}
		typ := l.resolveSourceTypePtr(expr.Args[0].Type, nil)
		if typ == "" {
			typ = l.expressionType(expr.Args[0], scope)
		}
		if _, _, ok := l.mapKeyValueTypes(typ); ok {
			if len(expr.Args) != 1 && len(expr.Args) != 2 {
				l.add("hirgen.builtin.arg_count", "make map requires an optional size", expr.Span)
				return ir.Expression{}, false
			}
			if !l.validateMapKeyComparable(typ, expr.Span) {
				return ir.Expression{}, false
			}
			mapExpr := ir.Expression{Kind: ir.ExprMap, Type: l.hirType(typ)}
			if len(expr.Args) == 2 {
				size, ok := l.lowerMakeIntegerArgument(expr.Args[1], scope)
				if !ok {
					return ir.Expression{}, false
				}
				mapExpr.Size = &size
			}
			return mapExpr, true
		}
		if l.isArrayType(typ) {
			l.add("hirgen.builtin.make.type", "make does not support arrays", expr.Span)
			return ir.Expression{}, false
		}
		if l.isSliceType(typ) {
			if len(expr.Args) != 2 && len(expr.Args) != 3 {
				l.add("hirgen.builtin.arg_count", "make slice requires length and optional capacity", expr.Span)
				return ir.Expression{}, false
			}
			args := make([]ir.Expression, 0, len(expr.Args)-1)
			for _, argument := range expr.Args[1:] {
				value, ok := l.lowerMakeIntegerArgument(argument, scope)
				if !ok {
					return ir.Expression{}, false
				}
				args = append(args, value)
			}
			return ir.Expression{Kind: ir.ExprMakeSlice, Type: l.hirType(typ), Args: args}, true
		}
		if l.isChannelType(typ) {
			channel, ok := l.channelTypeInfo(l.resolveNamedUnderlyingType(typ))
			if !ok || channel.direction != "both" {
				l.add("hirgen.builtin.make.type", "make channel requires a bidirectional channel type", expr.Args[0].Span)
				return ir.Expression{}, false
			}
			if len(expr.Args) > 2 {
				l.add("hirgen.builtin.arg_count", "make channel requires optional capacity", expr.Span)
				return ir.Expression{}, false
			}
			capacity := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: constant.Scalar(`0`)}
			if len(expr.Args) == 2 {
				var ok bool
				capacity, ok = l.lowerMakeIntegerArgument(expr.Args[1], scope)
				if !ok {
					return ir.Expression{}, false
				}
			}
			return ir.Expression{Kind: ir.ExprMakeChan, Type: l.hirType(typ), Args: []ir.Expression{capacity}}, true
		}
		l.add("hirgen.builtin.make.type", "make currently supports slices, maps, and channels", expr.Span)
		return ir.Expression{}, false
	default:
		l.add("hirgen.builtin.unsupported", "unsupported builtin call", expr.Span)
		return ir.Expression{}, false
	}
}

func (l *lowerer) lowerPrintBuiltinCall(name string, args []ir.Expression) ir.Expression {
	l.ensureSourceRequirement("fmt")
	packed := ir.Expression{Kind: ir.ExprSequence, Type: l.hirType("Slice<Any>"), Elements: args}
	return ir.Expression{
		Kind: ir.ExprCallDirect, ModulePath: "fmt", Function: "fn." + name,
		Args: []ir.Expression{packed}, ResultCount: 0,
	}
}

func (l *lowerer) validateComplexPartBuiltin(expr ast.Expression, scope *funcScope) bool {
	typ := l.expressionType(expr.Args[0], scope)
	if isComplexType(l.underlyingConstType(typ)) {
		return true
	}
	l.add("hirgen.builtin."+strings.TrimSpace(expr.Callee.Name)+".type", "real and imag arguments must be complex values", expr.Args[0].Span)
	return false
}

func (l *lowerer) lowerMakeIntegerArgument(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	typ := l.expressionType(expr, scope)
	if !isIntegerType(l.underlyingConstType(typ)) {
		l.add("hirgen.builtin.make.size.type", "make length and capacity arguments must be integer values", expr.Span)
		return ir.Expression{}, false
	}
	if raw, sourceType, ok := l.constValue(expr, scope); ok {
		constType := l.underlyingConstType(l.expressionType(expr, scope))
		if value, ok := l.constInt64(raw, constType); !ok {
			value, ok = l.constInt64(raw, sourceType)
			if ok && value < 0 {
				l.add("hirgen.builtin.make.size.negative", "make length and capacity arguments must be non-negative", expr.Span)
				return ir.Expression{}, false
			}
		} else if value < 0 {
			l.add("hirgen.builtin.make.size.negative", "make length and capacity arguments must be non-negative", expr.Span)
			return ir.Expression{}, false
		}
	}
	return l.lowerExpression(expr, scope)
}

func (l *lowerer) validateAppendBuiltin(expr ast.Expression, scope *funcScope) (string, bool) {
	if len(expr.Args) == 0 {
		l.add("hirgen.builtin.arg_count", "append builtin requires a target argument", expr.Span)
		return "", false
	}
	targetType := l.expressionType(expr.Args[0], scope)
	if !l.isSliceType(targetType) {
		l.add("hirgen.builtin.append.type", "append target must be a slice", expr.Args[0].Span)
		return "", false
	}
	elemType := l.indexElementType(targetType)
	if elemType == "" {
		l.add("hirgen.builtin.append.type", "append target must have a slice element type", expr.Args[0].Span)
		return "", false
	}
	if !expr.Ellipsis {
		return elemType, true
	}
	if len(expr.Args) != 2 {
		l.add("hirgen.builtin.append.ellipsis", "append ellipsis requires a target and one source slice", expr.Span)
		return "", false
	}
	sourceType := l.expressionType(expr.Args[1], scope)
	if l.isStringType(sourceType) && l.sameTypeIdentity(elemType, "Uint8") {
		return elemType, true
	}
	if !l.isSliceType(sourceType) {
		l.add("hirgen.builtin.append.ellipsis_type", "append ellipsis source must be a slice or string", expr.Args[1].Span)
		return "", false
	}
	sourceElemType := l.indexElementType(sourceType)
	if !l.sameTypeIdentity(sourceElemType, elemType) {
		l.add("hirgen.builtin.append.ellipsis_element", fmt.Sprintf("append ellipsis source element type %s does not match target element type %s", sourceElemType, elemType), expr.Args[1].Span)
		return "", false
	}
	return elemType, true
}

func (l *lowerer) validateDeleteBuiltin(expr ast.Expression, scope *funcScope) (string, bool) {
	if len(expr.Args) != 2 {
		l.add("hirgen.builtin.arg_count", "delete builtin requires map and key arguments", expr.Span)
		return "", false
	}
	keyType, _, ok := l.mapKeyValueTypes(l.expressionType(expr.Args[0], scope))
	if !ok {
		l.add("hirgen.builtin.delete.type", "delete target must be a map", expr.Args[0].Span)
		return "", false
	}
	if !l.validateMapKeyComparable(l.expressionType(expr.Args[0], scope), expr.Span) {
		return "", false
	}
	return keyType, true
}

func (l *lowerer) validateClearBuiltin(expr ast.Expression, scope *funcScope) bool {
	if len(expr.Args) != 1 {
		l.add("hirgen.builtin.arg_count", "clear builtin requires exactly one argument", expr.Span)
		return false
	}
	typ := l.expressionType(expr.Args[0], scope)
	if l.isSliceType(typ) {
		return true
	}
	if _, _, ok := l.mapKeyValueTypes(typ); ok {
		return l.validateMapKeyComparable(typ, expr.Span)
	}
	l.add("hirgen.builtin.clear.type", "clear target must be a map or slice", expr.Args[0].Span)
	return false
}

func (l *lowerer) validateCloseBuiltin(expr ast.Expression, scope *funcScope) bool {
	if len(expr.Args) != 1 {
		l.add("hirgen.builtin.arg_count", "close builtin requires exactly one argument", expr.Span)
		return false
	}
	typ := l.expressionType(expr.Args[0], scope)
	channel, ok := l.channelTypeInfo(l.resolveNamedUnderlyingType(typ))
	if !ok {
		l.add("hirgen.builtin.close.type", "close argument must be a channel", expr.Args[0].Span)
		return false
	}
	if channel.direction == "recv" {
		l.add("hirgen.builtin.close.direction", "close argument cannot be receive-only", expr.Args[0].Span)
		return false
	}
	return true
}

func (l *lowerer) validateCopyBuiltin(expr ast.Expression, scope *funcScope) bool {
	if len(expr.Args) != 2 {
		l.add("hirgen.builtin.arg_count", "copy builtin requires dst and src arguments", expr.Span)
		return false
	}
	dstType := l.expressionType(expr.Args[0], scope)
	srcType := l.expressionType(expr.Args[1], scope)
	if !l.isSliceType(dstType) {
		l.add("hirgen.builtin.copy.type", "copy destination must be a slice", expr.Args[0].Span)
		return false
	}
	if l.isStringType(srcType) {
		if !l.sameTypeIdentity(l.indexElementType(dstType), "Uint8") {
			l.add("hirgen.builtin.copy.type", "copy from string requires a byte slice destination", expr.Args[0].Span)
			return false
		}
		return true
	}
	if !l.isSliceType(srcType) {
		l.add("hirgen.builtin.copy.type", "copy source must be a slice or string", expr.Args[1].Span)
		return false
	}
	if !l.sameTypeIdentity(l.indexElementType(dstType), l.indexElementType(srcType)) {
		l.add("hirgen.builtin.copy.element", "copy source and destination must have identical element types", expr.Span)
		return false
	}
	return true
}
