package lower

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) lowerRuntimeConstant(expr ast.Expression, raw json.RawMessage, typ string, scope *funcScope) (ir.Expression, bool) {
	if !l.untypedConstExpression(expr, scope) {
		return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(typ), Value: append(json.RawMessage(nil), raw...)}, true
	}
	target := defaultUntypedConstType(typ, raw)
	if target == "" || target == "Any" {
		target = inferConstRawDefaultType(raw)
	}
	converted, convertedType, ok := l.convertTypedConstValue(raw, typ, target, expr.Span)
	if !ok {
		return ir.Expression{}, false
	}
	return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(convertedType), Value: converted}, true
}

func (l *lowerer) lowerExpressionInType(expr ast.Expression, targetType string, scope *funcScope) (ir.Expression, bool) {
	return l.lowerExpressionInTypeOptions(expr, targetType, scope, false)
}

func (l *lowerer) lowerCompositeValueInType(expr ast.Expression, targetType string, scope *funcScope) (ir.Expression, bool) {
	return l.lowerExpressionInTypeOptions(expr, targetType, scope, true)
}

func (l *lowerer) lowerExpressionInTypeOptions(expr ast.Expression, targetType string, scope *funcScope, allowElidedComposite bool) (ir.Expression, bool) {
	targetType = l.resolveType(strings.TrimSpace(targetType))
	if l.suppressConstantArithmetic == 0 && !l.validateConstantArithmetic(expr, scope) {
		return ir.Expression{}, false
	}
	if targetType != "" && l.isNilAssignableType(targetType) && isNilLiteral(expr) {
		return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(l.resolveType(targetType)), Value: json.RawMessage("null")}, true
	}
	if expr.Kind == ast.ExprComposite && (expr.Type == nil || expr.Type.Kind == ast.TypeInvalid) {
		if !allowElidedComposite {
			l.add("hirgen.composite.elided_context", "elided composite literal type is only valid inside another composite literal", expr.Span)
			return ir.Expression{}, false
		}
		return l.lowerElidedCompositeInType(expr, targetType, scope)
	}
	if lowered, handled, ok := l.lowerUntypedConstInterfaceDefault(expr, targetType, scope); handled {
		return lowered, ok
	}
	if targetType == "Any" {
		if raw, sourceType, ok := l.constValue(expr, scope); ok {
			literal := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(sourceType), Value: raw}
			return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType("Any"), Operand: &literal}, true
		}
		lowered, ok := l.lowerExpression(expr, scope)
		if !ok {
			return ir.Expression{}, false
		}
		if hirResultCount(lowered) != 1 {
			return lowered, true
		}
		if l.typeRefString(lowered.Type) == "Any" {
			return lowered, true
		}
		return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType("Any"), Operand: &lowered}, true
	}
	if targetType != "" && targetType != "Any" {
		raw, sourceType, ok := l.constValue(expr, scope)
		if ok {
			if !l.constRepresentabilityTarget(targetType) {
				return l.lowerExpression(expr, scope)
			}
			converted, typ, ok := l.convertTypedConstValue(raw, sourceType, targetType, expr.Span)
			if !ok {
				return ir.Expression{}, false
			}
			return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(typ), Value: converted}, true
		}
	}
	if targetType != "" && l.semantic != nil && expr.Kind == ast.ExprBinary && !isEqualityOperator(expr.Operator) && !isOrderedComparisonOperator(expr.Operator) && !isLogicalOperator(expr.Operator) && expr.Left != nil && expr.Right != nil {
		info, found := l.semantic.Exprs[expr.NodeID]
		if found && info.Untyped && info.Mode != check.ExprConstant {
			_, _, rightTarget, ok := l.semanticBinaryFact(expr)
			if !ok {
				return ir.Expression{}, false
			}
			if !isShiftOperator(expr.Operator) {
				rightTarget = targetType
			}
			left, ok := l.lowerExpressionInType(*expr.Left, targetType, scope)
			if !ok {
				return ir.Expression{}, false
			}
			right, ok := l.lowerExpressionInType(*expr.Right, rightTarget, scope)
			if !ok {
				return ir.Expression{}, false
			}
			return ir.Expression{Kind: ir.ExprBinary, Operator: expr.Operator, Type: l.hirType(targetType), Left: &left, Right: &right}, true
		}
	}
	lowered, ok := l.lowerExpression(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	if targetType == "" || hirResultCount(lowered) != 1 {
		return lowered, true
	}
	sourceType := strings.TrimSpace(l.expressionType(expr, scope))
	if expr.Kind == ast.ExprConvert || expr.Kind == ast.ExprAssert {
		if loweredType := strings.TrimSpace(l.typeRefString(lowered.Type)); loweredType != "" {
			sourceType = loweredType
		}
	}
	if sourceType == "" {
		sourceType = strings.TrimSpace(l.typeRefString(lowered.Type))
	}
	relation, structured := l.semanticAssignmentRelation(expr, -1, targetType)
	if !structured && sourceType != "" {
		relation = l.assignmentRelation(sourceType, targetType)
	}
	if (structured || sourceType != "") && !relation.OK {
		l.add("hirgen.assign.type", fmt.Sprintf("cannot assign %s to %s", sourceType, targetType), expr.Span)
		return ir.Expression{}, false
	}
	return lowered, true
}

func (l *lowerer) lowerUntypedConstInterfaceDefault(expr ast.Expression, targetType string, scope *funcScope) (ir.Expression, bool, bool) {
	if targetType == "" || !l.isInterfaceValueType(targetType) {
		return ir.Expression{}, false, false
	}
	if isNilLiteral(expr) {
		return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(targetType), Value: json.RawMessage("null")}, true, true
	}
	if !l.untypedConstExpression(expr, scope) {
		return ir.Expression{}, false, false
	}
	raw, sourceType, ok := l.constValue(expr, scope)
	if !ok {
		return ir.Expression{}, false, false
	}
	defaultType := defaultUntypedConstType(sourceType, raw)
	if defaultType == "" || defaultType == "Any" {
		defaultType = inferConstRawDefaultType(raw)
	}
	converted, typ, ok := l.convertTypedConstValue(raw, sourceType, defaultType, expr.Span)
	if !ok {
		return ir.Expression{}, true, false
	}
	literal := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(typ), Value: converted}
	if targetType == "Any" {
		return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType("Any"), Operand: &literal}, true, true
	}
	if !l.assignableType(typ, targetType) {
		l.add("hirgen.assign.type", fmt.Sprintf("cannot assign %s to %s", typ, targetType), expr.Span)
		return ir.Expression{}, true, false
	}
	return literal, true, true
}

func (l *lowerer) lowerIndexExpression(expr ast.Expression, targetType string, scope *funcScope) (ir.Expression, bool) {
	targetType = strings.TrimSpace(targetType)
	if targetType == "" {
		targetType = "Int"
	}
	if targetType != "Int" {
		return l.lowerExpressionInType(expr, targetType, scope)
	}
	if _, _, ok := l.constValue(expr, scope); ok {
		return l.lowerExpressionInType(expr, "Int", scope)
	}
	lowered, ok := l.lowerExpression(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	sourceType := strings.TrimSpace(l.expressionType(expr, scope))
	if expr.Kind == ast.ExprConvert || expr.Kind == ast.ExprAssert {
		if loweredType := strings.TrimSpace(l.typeRefString(lowered.Type)); loweredType != "" {
			sourceType = loweredType
		}
	}
	if sourceType == "" {
		sourceType = strings.TrimSpace(l.typeRefString(lowered.Type))
	}
	if l.resolveNamedUnderlyingType(sourceType) == "Int" {
		return lowered, true
	}
	if isIntegerType(l.resolveNamedUnderlyingType(sourceType)) {
		return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType("Int"), Operand: &lowered}, true
	}
	l.add("hirgen.index.type", "index expression must be integer, got "+firstNonEmpty(sourceType, "unknown"), expr.Span)
	return ir.Expression{}, false
}

func (l *lowerer) validateConstantIndexRange(expr ast.Expression, scope *funcScope) bool {
	if expr.Operand == nil || expr.Index == nil {
		return true
	}
	objectType := l.expressionType(*expr.Operand, scope)
	if _, _, ok := l.mapKeyValueTypes(objectType); ok {
		return true
	}
	index, ok := l.constantIndexValue(*expr.Index, scope)
	if !ok {
		return true
	}
	if index < 0 {
		l.add("hirgen.index.negative", "constant index must be non-negative", expr.Index.Span)
		return false
	}
	if length, ok := l.constantIndexBound(*expr.Operand, objectType, scope); ok && uint64(index) >= length {
		l.add("hirgen.index.range", "constant index is out of range", expr.Index.Span)
		return false
	}
	return true
}

func (l *lowerer) constantIndexValue(expr ast.Expression, scope *funcScope) (int64, bool) {
	raw, typ, ok := l.constValue(expr, scope)
	if !ok {
		return 0, false
	}
	typ = strings.TrimSpace(typ)
	if typ == "" || typ == "Any" {
		typ = inferConstRawDefaultType(raw)
	}
	if value, ok := l.constExactInteger(raw, typ); ok {
		if constant.CompareSignedDecimal(value, minInt64Text) < 0 {
			return minInt64Value, true
		}
		if constant.CompareSignedDecimal(value, maxInt64Text) > 0 {
			return maxInt64Value, true
		}
		return constant.SignedDecimalInt64(value)
	}
	return 0, false
}

func (l *lowerer) constantIndexBound(operand ast.Expression, objectType string, scope *funcScope) (uint64, bool) {
	if length, _, ok := l.arrayTypeInfo(objectType); ok {
		return uint64(length), length >= 0
	}
	if arrayType, ok := l.pointerArrayType(objectType); ok {
		length, _, ok := l.arrayTypeInfo(arrayType)
		return uint64(length), ok && length >= 0
	}
	if !l.isStringType(objectType) {
		return 0, false
	}
	raw, typ, ok := l.constValue(operand, scope)
	if !ok {
		return 0, false
	}
	text, ok := l.constString(raw, typ)
	if !ok {
		return 0, false
	}
	return uint64(len([]byte(text))), true
}

func (l *lowerer) validateFullSliceExpression(expr ast.Expression, scope *funcScope) bool {
	if expr.Operand == nil || expr.Max == nil {
		return true
	}
	objectType := l.expressionType(*expr.Operand, scope)
	if l.isStringType(objectType) {
		l.add("hirgen.slice.full_string", "full slice expression is not valid for strings", expr.Span)
		return false
	}
	if !l.isSliceType(objectType) && !l.isArrayType(objectType) && !l.pointerArrayTypeOK(objectType) {
		l.add("hirgen.slice.full_type", "full slice expression requires an array, pointer to array, or slice operand", expr.Span)
		return false
	}
	return l.validateConstantFullSliceBounds(expr, objectType, scope)
}

func (l *lowerer) validateConstantFullSliceBounds(expr ast.Expression, objectType string, scope *funcScope) bool {
	low, lowOK := int64(0), true
	lowSpan := expr.Span
	if expr.Start != nil {
		var ok bool
		low, ok = l.constantIndexValue(*expr.Start, scope)
		lowOK = ok
		lowSpan = expr.Start.Span
	}
	high, highOK := int64(0), false
	highSpan := expr.Span
	if expr.End != nil {
		var ok bool
		high, ok = l.constantIndexValue(*expr.End, scope)
		highOK = ok
		highSpan = expr.End.Span
	}
	maxIndex, maxOK := l.constantIndexValue(*expr.Max, scope)
	if lowOK && low < 0 {
		l.add("hirgen.slice.negative", "constant slice index must be non-negative", lowSpan)
		return false
	}
	if highOK && high < 0 {
		l.add("hirgen.slice.negative", "constant slice index must be non-negative", highSpan)
		return false
	}
	if maxOK && maxIndex < 0 {
		l.add("hirgen.slice.negative", "constant slice index must be non-negative", expr.Max.Span)
		return false
	}
	if lowOK && highOK && low > high {
		l.add("hirgen.slice.order", "constant full slice indexes must satisfy low <= high <= max", expr.Span)
		return false
	}
	if highOK && maxOK && high > maxIndex {
		l.add("hirgen.slice.order", "constant full slice indexes must satisfy low <= high <= max", expr.Span)
		return false
	}
	bound, boundOK := l.constantIndexBound(*expr.Operand, objectType, scope)
	if !boundOK {
		return true
	}
	if lowOK && uint64(low) > bound {
		l.add("hirgen.slice.range", "constant slice index is out of range", lowSpan)
		return false
	}
	if highOK && uint64(high) > bound {
		l.add("hirgen.slice.range", "constant slice index is out of range", highSpan)
		return false
	}
	if maxOK && uint64(maxIndex) > bound {
		l.add("hirgen.slice.range", "constant slice index is out of range", expr.Max.Span)
		return false
	}
	return true
}

func (l *lowerer) lowerElidedCompositeInType(expr ast.Expression, targetType string, scope *funcScope) (ir.Expression, bool) {
	targetType = l.resolveType(strings.TrimSpace(targetType))
	if targetType == "" || targetType == "Any" {
		l.add("hirgen.composite.type", "composite literal requires a target type", expr.Span)
		return ir.Expression{}, false
	}
	if elemType, ok := l.pointerElementType(targetType); ok {
		composite := expr
		typ := l.inferredCompositeTypeExpr(elemType, expr.Span)
		composite.Type = &typ
		value, ok := l.lowerComposite(composite, scope)
		if !ok {
			return ir.Expression{}, false
		}
		local := l.newSyntheticLocal(scope, "composite.ptr", elemType)
		addr := ir.Expression{Kind: ir.ExprAddressOf, Local: local}
		return ir.Expression{Kind: ir.ExprLet, Local: local, Bind: &value, Body: &addr}, true
	}
	composite := expr
	typ := l.inferredCompositeTypeExpr(targetType, expr.Span)
	composite.Type = &typ
	return l.lowerComposite(composite, scope)
}

func (l *lowerer) lowerModuleTypeConversionCall(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	if expr.Callee == nil || expr.Callee.Kind != ast.ExprSelector {
		return ir.Expression{}, false
	}
	if _, ok := l.selectorTypeExport(*expr.Callee, scope); !ok {
		return ir.Expression{}, false
	}
	if expr.Ellipsis {
		l.add("hirgen.convert.ellipsis", "type conversion cannot use ellipsis", expr.Span)
		return ir.Expression{}, true
	}
	if len(expr.Args) != 1 {
		l.add("hirgen.convert.arg_count", "type conversion requires exactly one argument", expr.Span)
		return ir.Expression{}, true
	}
	target := l.selectorConversionType(*expr.Callee, scope)
	if isNilLiteral(expr.Args[0]) && l.isNilAssignableType(target) {
		return ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType(target), Value: json.RawMessage("null")}, true
	}
	operand, ok := l.lowerExpression(expr.Args[0], scope)
	if !ok {
		return ir.Expression{}, true
	}
	if !l.validateConversion(expr.Args[0], target, scope, expr.Span) {
		return ir.Expression{}, false
	}
	return ir.Expression{Kind: ir.ExprConvert, Type: l.hirType(target), Operand: &operand}, true
}
