package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) lowerMinMaxCall(expr ast.Expression, scope *funcScope) (ir.Expression, bool) {
	if expr.Ellipsis {
		l.add("hirgen.builtin.minmax.ellipsis", "min and max do not permit slice ellipsis", expr.Span)
		return ir.Expression{}, false
	}
	targetType, ok := l.minMaxTargetType(expr, scope)
	if !ok {
		return ir.Expression{}, false
	}
	packedType := "Slice<" + targetType + ">"
	valueTypeExpr := ast.TypeExpr{Kind: ast.TypeName, Name: targetType}
	valuesTypeExpr := ast.TypeExpr{Kind: ast.TypeSlice, Elem: &valueTypeExpr}
	values := ast.Expression{
		Kind: ast.ExprIdent,
		Name: "values",
		Type: &valuesTypeExpr,
	}
	zero := ast.Expression{
		Kind:    ast.ExprLiteral,
		Literal: "0",
		Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: "Int"},
	}
	best := ast.Expression{Kind: ast.ExprIdent, Name: "best", Type: &valueTypeExpr}
	value := ast.Expression{Kind: ast.ExprIdent, Name: "value", Type: &valueTypeExpr}
	first := ast.Expression{Kind: ast.ExprIndex, Operand: &values, Index: &zero, Type: &valueTypeExpr}
	comparisonOperator := "<"
	if expr.Callee != nil && strings.TrimSpace(expr.Callee.Name) == "max" {
		comparisonOperator = ">"
	}
	comparison := l.minMaxUpdateCondition(comparisonOperator, targetType, value, best)
	update := ast.Statement{
		Kind:  ast.StmtAssign,
		Op:    "=",
		Left:  []ast.Expression{best},
		Right: []ast.Expression{value},
	}
	rangeStmt := ast.Statement{
		Kind:  ast.StmtRange,
		Op:    ":=",
		Key:   &ast.Expression{Kind: ast.ExprIdent, Name: "_"},
		Value: &ast.Expression{Kind: ast.ExprIdent, Name: "value", Type: &valueTypeExpr},
		Range: &values,
		Body: ast.BlockStmt{Stmts: []ast.Statement{{
			Kind: ast.StmtIf,
			Cond: &comparison,
			Body: ast.BlockStmt{Stmts: []ast.Statement{update}},
		}}},
	}
	generated := ast.Expression{
		Kind: ast.ExprFunc,
		Func: &ast.FuncDecl{
			Params:  []ast.Field{{Name: "values", Type: valueTypeExpr, Variadic: true}},
			Results: []ast.Field{{Type: valueTypeExpr}},
			Body: ast.BlockStmt{Stmts: []ast.Statement{
				{Kind: ast.StmtDecl, Decls: []ast.Decl{{Kind: ast.DeclVar, Var: &ast.ValueDecl{
					Names: []string{"best"}, Type: valueTypeExpr, Values: []ast.Expression{first},
				}}}},
				rangeStmt,
				{Kind: ast.StmtReturn, Results: []ast.Expression{best}},
			}},
		},
	}
	fnValue, ok := l.lowerFuncLiteral(generated, nil)
	if !ok {
		return ir.Expression{}, false
	}
	l.extraFunctions[len(l.extraFunctions)-1].Generated = true
	callArgs := make([]ir.Expression, 0, 1)
	elements := make([]ir.Expression, 0, len(expr.Args))
	for _, arg := range expr.Args {
		lowered, ok := l.lowerExpressionInType(arg, targetType, scope)
		if !ok {
			return ir.Expression{}, false
		}
		elements = append(elements, lowered)
	}
	callArgs = append(callArgs, ir.Expression{Kind: ir.ExprSequence, Type: l.hirType(packedType), Elements: elements})
	return ir.Expression{
		Kind:        ir.ExprCallDirect,
		Function:    fnValue.Function,
		Args:        callArgs,
		Type:        l.hirType(targetType),
		ResultCount: 1,
	}, true
}

func (l *lowerer) minMaxUpdateCondition(operator, targetType string, value, best ast.Expression) ast.Expression {
	normal := astBinary(operator, value, best)
	if !isFloatType(l.resolveNamedUnderlyingType(targetType)) {
		return normal
	}
	valueNaN := astBinary("!=", value, value)
	bestNaN := astBinary("!=", best, best)
	notBestNaN := ast.Expression{Kind: ast.ExprUnary, Operator: "!", Operand: &bestNaN}
	valueZero := astBinary("==", value, typedNumericLiteral(targetType, "0"))
	bestZero := astBinary("==", best, typedNumericLiteral(targetType, "0"))
	bothZero := astBinary("&&", valueZero, bestZero)
	valueReciprocal := astArithmetic("/", typedNumericLiteral(targetType, "1"), value, targetType)
	bestReciprocal := astArithmetic("/", typedNumericLiteral(targetType, "1"), best, targetType)
	zeroOperator := "<"
	if operator == ">" {
		zeroOperator = ">"
	}
	zeroBetter := astBinary(zeroOperator, valueReciprocal, bestReciprocal)
	tiebreak := astBinary("&&", bothZero, zeroBetter)
	ordinaryOrZero := astBinary("||", normal, tiebreak)
	eligible := astBinary("&&", notBestNaN, ordinaryOrZero)
	return astBinary("||", valueNaN, eligible)
}

func astBinary(operator string, left, right ast.Expression) ast.Expression {
	return ast.Expression{
		Kind:     ast.ExprBinary,
		Operator: operator,
		Left:     &left,
		Right:    &right,
		Type:     &ast.TypeExpr{Kind: ast.TypeName, Name: "Bool"},
	}
}

func astArithmetic(operator string, left, right ast.Expression, typ string) ast.Expression {
	return ast.Expression{
		Kind:     ast.ExprBinary,
		Operator: operator,
		Left:     &left,
		Right:    &right,
		Type:     &ast.TypeExpr{Kind: ast.TypeName, Name: typ},
	}
}

func typedNumericLiteral(targetType, literal string) ast.Expression {
	value := ast.Expression{
		Kind:    ast.ExprLiteral,
		Literal: literal,
		Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: "Int"},
	}
	return ast.Expression{
		Kind:    ast.ExprConvert,
		Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: targetType},
		Operand: &value,
	}
}

func (l *lowerer) minMaxTargetType(expr ast.Expression, scope *funcScope) (string, bool) {
	if len(expr.Args) == 0 {
		l.add("hirgen.builtin.minmax.arg_count", "min and max require at least one argument", expr.Span)
		return "", false
	}
	if expr.Ellipsis {
		return "", false
	}
	target := ""
	untypedTypes := make([]string, 0, len(expr.Args))
	for _, arg := range expr.Args {
		typ := strings.TrimSpace(l.expressionType(arg, scope))
		if typ == "" || typ == "Any" {
			l.add("hirgen.builtin.minmax.type", "min and max arguments require ordered values", arg.Span)
			return "", false
		}
		if l.untypedConstExpression(arg, scope) {
			untypedTypes = append(untypedTypes, typ)
			continue
		}
		if target == "" {
			target = typ
			continue
		}
		if typ != target {
			l.add("hirgen.builtin.minmax.types", "min and max arguments must have identical types", arg.Span)
			return "", false
		}
	}
	if target == "" {
		inferred, ok := commonMinMaxUntypedType(untypedTypes)
		if !ok {
			l.add("hirgen.builtin.minmax.types", "min and max untyped arguments must have a common ordered type", expr.Span)
			return "", false
		}
		target = inferred
	}
	underlying := l.resolveNamedUnderlyingType(target)
	for _, typ := range untypedTypes {
		if !minMaxUntypedAssignable(typ, underlying) {
			l.add("hirgen.builtin.minmax.types", "min and max arguments must have compatible ordered types", expr.Span)
			return "", false
		}
	}
	if !l.validateMinMaxType(target, expr.Span) {
		return "", false
	}
	return target, true
}

func commonMinMaxUntypedType(types []string) (string, bool) {
	if len(types) == 0 {
		return "", false
	}
	hasFloat := false
	for _, typ := range types {
		typ = strings.TrimSpace(typ)
		switch {
		case typ == "String":
			if hasFloat {
				return "", false
			}
		case isFloatType(typ):
			hasFloat = true
		case isIntegerType(typ):
		default:
			return "", false
		}
	}
	if hasFloat {
		for _, typ := range types {
			if strings.TrimSpace(typ) == "String" {
				return "", false
			}
		}
		return "Float64", true
	}
	for _, typ := range types {
		if strings.TrimSpace(typ) == "String" {
			return "String", true
		}
	}
	return "Int", true
}

func minMaxUntypedAssignable(source, target string) bool {
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	if target == "String" {
		return source == "String"
	}
	if isFloatType(target) {
		return isIntegerType(source) || isFloatType(source)
	}
	if isIntegerType(target) {
		return isIntegerType(source)
	}
	return false
}

func (l *lowerer) validateMinMaxType(typ string, span source.Span) bool {
	underlying := l.resolveNamedUnderlyingType(strings.TrimSpace(typ))
	if underlying == "String" || isIntegerType(underlying) || isFloatType(underlying) {
		return true
	}
	l.add("hirgen.builtin.minmax.type", "min and max require integer, floating-point, or string arguments", span)
	return false
}
