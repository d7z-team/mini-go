package lower

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) lowerBinaryOperands(expr ast.Expression, scope *funcScope) (ir.Expression, ir.Expression, bool) {
	if expr.Left == nil || expr.Right == nil {
		l.add("hirgen.binary.operand", "binary expression requires two operands", expr.Span)
		return ir.Expression{}, ir.Expression{}, false
	}
	_, semanticLeftTarget, semanticRightTarget, semanticFact := l.semanticBinaryFact(expr)
	if isLogicalOperator(expr.Operator) {
		left, ok := l.lowerBoolOperand(*expr.Left, scope)
		if !ok {
			return ir.Expression{}, ir.Expression{}, false
		}
		deadRight := false
		if raw, typ, constant := l.constValue(*expr.Left, scope); constant {
			if value, ok := l.constBool(raw, typ); ok {
				deadRight = expr.Operator == "&&" && !value
				deadRight = deadRight || expr.Operator == "||" && value
			}
		}
		if deadRight {
			l.suppressConstantArithmetic++
		}
		right, ok := l.lowerBoolOperand(*expr.Right, scope)
		if deadRight {
			l.suppressConstantArithmetic--
		}
		if !ok {
			return ir.Expression{}, ir.Expression{}, false
		}
		return left, right, true
	}
	if isEqualityOperator(expr.Operator) {
		leftNil := isNilLiteral(*expr.Left)
		rightNil := isNilLiteral(*expr.Right)
		if leftNil && !rightNil {
			rightType := l.expressionType(*expr.Right, scope)
			if l.isNilAssignableType(rightType) {
				left, ok := l.lowerExpressionInType(*expr.Left, rightType, scope)
				if !ok {
					return ir.Expression{}, ir.Expression{}, false
				}
				right, ok := l.lowerExpression(*expr.Right, scope)
				if !ok {
					return ir.Expression{}, ir.Expression{}, false
				}
				return left, right, true
			}
		}
		if rightNil && !leftNil {
			leftType := l.expressionType(*expr.Left, scope)
			if l.isNilAssignableType(leftType) {
				left, ok := l.lowerExpression(*expr.Left, scope)
				if !ok {
					return ir.Expression{}, ir.Expression{}, false
				}
				right, ok := l.lowerExpressionInType(*expr.Right, leftType, scope)
				if !ok {
					return ir.Expression{}, ir.Expression{}, false
				}
				return left, right, true
			}
		}
		if !leftNil && !rightNil {
			leftType := l.expressionType(*expr.Left, scope)
			rightType := l.expressionType(*expr.Right, scope)
			if l.isInterfaceValueType(leftType) || l.isInterfaceValueType(rightType) {
				left, ok := l.lowerExpression(*expr.Left, scope)
				if !ok {
					return ir.Expression{}, ir.Expression{}, false
				}
				right, ok := l.lowerExpression(*expr.Right, scope)
				if !ok {
					return ir.Expression{}, ir.Expression{}, false
				}
				return left, right, true
			}
		}
	}
	leftTarget, rightTarget := semanticLeftTarget, semanticRightTarget
	if !semanticFact {
		leftTarget, rightTarget = l.binaryOperandTargetTypes(expr.Operator, *expr.Left, *expr.Right, scope)
	}
	left, ok := l.lowerExpressionInType(*expr.Left, leftTarget, scope)
	if !ok {
		return ir.Expression{}, ir.Expression{}, false
	}
	right, ok := l.lowerExpressionInType(*expr.Right, rightTarget, scope)
	if !ok {
		return ir.Expression{}, ir.Expression{}, false
	}
	return left, right, true
}

func (l *lowerer) validateConstantArithmetic(expr ast.Expression, scope *funcScope) bool {
	if expr.Kind != ast.ExprBinary || expr.Left == nil || expr.Right == nil {
		return true
	}
	if isShiftOperator(expr.Operator) {
		if count, ok := constantShiftCountFromExpression(*expr.Right); ok {
			if count < 0 {
				l.add("hirgen.const.shift.negative", "constant shift count must be non-negative", expr.Right.Span)
				return false
			}
			if count > 4096 {
				l.add("hirgen.const.shift.count", "constant shift count is too large to fold", expr.Right.Span)
				return false
			}
		}
	}
	rightRaw, rightType, rightOK := l.constValue(derefExpr(expr.Right), scope)
	if !rightOK {
		return true
	}
	if expr.Operator == "/" || expr.Operator == "%" {
		if !l.constantNumericZero(rightRaw, rightType) {
			return true
		}
		l.add("hirgen.const.div_zero", "constant arithmetic has a zero divisor", expr.Span)
		return false
	}
	if isShiftOperator(expr.Operator) {
		if negative, ok := l.constantShiftCountNegative(rightRaw, rightType); ok && negative {
			l.add("hirgen.const.shift.negative", "constant shift count must be non-negative", expr.Right.Span)
			return false
		}
		if tooLarge, ok := l.constantShiftCountTooLarge(rightRaw, rightType); ok && tooLarge {
			l.add("hirgen.const.shift.count", "constant shift count is too large to fold", expr.Right.Span)
			return false
		}
	}
	return true
}

func (l *lowerer) constantNumericZero(raw *constant.Value, typ string) bool {
	if !isNumericType(l.underlyingConstType(typ)) {
		return false
	}
	value, ok := l.constExactComplex(raw, typ)
	return ok && value.realPart.IsZero() && value.imaginaryPart.IsZero()
}

func (l *lowerer) constantShiftCountNegative(raw *constant.Value, typ string) (bool, bool) {
	if !isIntegerType(l.underlyingConstType(typ)) {
		return false, false
	}
	value, ok := l.constExactInteger(raw, typ)
	if !ok {
		return false, false
	}
	return strings.HasPrefix(value, "-"), true
}

func (l *lowerer) constantShiftCountTooLarge(raw *constant.Value, typ string) (bool, bool) {
	if !isIntegerType(l.underlyingConstType(typ)) {
		return false, false
	}
	value, ok := l.constExactInteger(raw, typ)
	if !ok {
		return false, false
	}
	if strings.HasPrefix(value, "-") {
		return false, true
	}
	return constant.CompareUnsignedDecimal(value, "4096") > 0, true
}

func constantShiftCountFromExpression(expr ast.Expression) (int64, bool) {
	switch expr.Kind {
	case ast.ExprLiteral:
		text := strings.TrimSpace(expr.Literal)
		if text == "" || strings.ContainsAny(text, ".eEiI") {
			return 0, false
		}
		value, err := strconv.ParseInt(text, 0, 64)
		if err == nil {
			return value, true
		}
		unsigned, err := strconv.ParseUint(text, 0, 64)
		if err != nil || unsigned > uint64(maxInt64Value) {
			return maxInt64Value, err == nil
		}
		return int64(unsigned), true
	case ast.ExprUnary:
		if expr.Operand == nil {
			return 0, false
		}
		value, ok := constantShiftCountFromExpression(*expr.Operand)
		if !ok {
			return 0, false
		}
		switch strings.TrimSpace(expr.Operator) {
		case "+":
			return value, true
		case "-":
			return -value, true
		default:
			return 0, false
		}
	default:
		return 0, false
	}
}

func (l *lowerer) lowerBoolOperand(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	out, ok := l.lowerExpression(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	typ := l.expressionType(expr, scope)
	if typ != "" && l.resolveType(typ) != "Bool" && l.resolveNamedUnderlyingType(typ) == "Bool" {
		operand := out
		out = ir.Expression{Kind: ir.ExprConvert, Type: l.hirType("Bool"), Operand: &operand}
	}
	return out, true
}

func (l *lowerer) binaryOperandTargetTypes(operator string, left, right ast.Expression, scope *funcScope) (string, string) {
	if isLogicalOperator(operator) {
		return "", ""
	}
	leftType := l.expressionType(left, scope)
	rightType := l.expressionType(right, scope)
	if isShiftOperator(operator) {
		return "", ""
	}
	if isNumericOperandOperator(operator) && leftType != "" && rightType != "" && leftType != "Any" && rightType != "Any" {
		if l.isUntypedNumericExpression(left, leftType, scope) && isNumericType(l.underlyingConstType(rightType)) {
			return rightType, rightType
		}
		if l.isUntypedNumericExpression(right, rightType, scope) && isNumericType(l.underlyingConstType(leftType)) {
			return leftType, leftType
		}
	}
	if leftType != "" && leftType != "Any" {
		rightType = leftType
	}
	if rightType != "" && rightType != "Any" {
		leftType = rightType
	}
	return leftType, rightType
}

func (l *lowerer) binaryExpressionType(expr ast.Expression, scope *funcScope) string {
	if result, _, _, ok := l.semanticBinaryFact(expr); ok {
		return result
	}
	if expr.Left == nil || expr.Right == nil {
		return ""
	}
	if isEqualityOperator(expr.Operator) || isOrderedComparisonOperator(expr.Operator) || isLogicalOperator(expr.Operator) {
		return "Bool"
	}
	leftType := l.expressionType(*expr.Left, scope)
	rightType := l.expressionType(*expr.Right, scope)
	if isNumericOperandOperator(expr.Operator) && leftType != "" && rightType != "" && leftType != "Any" && rightType != "Any" {
		if l.isUntypedNumericExpression(*expr.Left, leftType, scope) && isNumericType(l.underlyingConstType(rightType)) {
			return rightType
		}
		if l.isUntypedNumericExpression(*expr.Right, rightType, scope) && isNumericType(l.underlyingConstType(leftType)) {
			return leftType
		}
	}
	if leftType != "" && leftType != "Any" {
		return leftType
	}
	return rightType
}

func isNumericOperandOperator(operator string) bool {
	if isEqualityOperator(operator) || isOrderedComparisonOperator(operator) {
		return true
	}
	switch strings.TrimSpace(operator) {
	case "+", "-", "*", "/", "%", "&", "|", "^", "&^", "<<", ">>":
		return true
	default:
		return false
	}
}

func (l *lowerer) isUntypedNumericExpression(expr ast.Expression, typ string, scope *funcScope) bool {
	if !isNumericType(l.underlyingConstType(strings.TrimSpace(typ))) {
		return false
	}
	if untyped, ok := l.semanticUntypedConstant(expr); ok {
		return untyped
	}
	switch expr.Kind {
	case ast.ExprLiteral:
		return true
	case ast.ExprIdent:
		value, ok := l.lookupConstValue(expr.Name, scope)
		return ok && value.Untyped
	case ast.ExprSelector:
		export, ok := l.selectorExport(expr, scope)
		return ok && export.Kind == check.ObjectConst && export.Untyped
	case ast.ExprUnary:
		if expr.Operand == nil {
			return false
		}
		return l.isUntypedNumericExpression(*expr.Operand, l.expressionType(*expr.Operand, scope), scope)
	case ast.ExprBinary:
		if expr.Left == nil || expr.Right == nil || !l.isUntypedNumericExpression(*expr.Left, l.expressionType(*expr.Left, scope), scope) || !l.isUntypedNumericExpression(*expr.Right, l.expressionType(*expr.Right, scope), scope) {
			return false
		}
		_, _, ok := l.constValue(expr, scope)
		return ok
	default:
		return false
	}
}
