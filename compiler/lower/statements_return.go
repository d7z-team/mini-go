package lower

import (
	"fmt"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) lowerReturn(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	if scope != nil && scope.rangeReturn != nil {
		return l.lowerRangeFunctionReturn(stmt, scope)
	}
	out := ir.Statement{Kind: ir.StmtReturn}
	expectedReturns := 0
	var resultTypes []string
	var namedResults []ir.Expression
	if scope != nil {
		expectedReturns = scope.expectedReturns
		resultTypes = l.typeStringsFromRefs(scope.resultTypes)
		namedResults = scope.namedResults
	}
	if len(stmt.Results) == 0 {
		if len(namedResults) != 0 {
			if !l.validateBareReturnNamedResults(scope, stmt.Span) {
				return nil, false
			}
			out.Results = append(out.Results, namedResults...)
		} else if expectedReturns != 0 {
			l.add("hirgen.return.bare", "bare return requires named results", stmt.Span)
			return nil, false
		}
		return []ir.Statement{
			out,
			{Kind: ir.StmtLabel, Label: l.newLabel("return.after")},
		}, true
	}
	if expectedReturns == 0 {
		l.add("hirgen.return.results", "return result count does not match function results", stmt.Span)
		return nil, false
	}
	prelude, values, ok := l.lowerReturnValueList(stmt.Results, resultTypes, scope)
	if !ok {
		return nil, false
	}
	if hirResultsCount(values) != expectedReturns {
		l.add("hirgen.return.results", "return result count does not match function results", stmt.Span)
		return nil, false
	}
	if len(namedResults) != 0 {
		targets := make([]ir.StoreTarget, 0, len(namedResults))
		for _, result := range namedResults {
			targets = append(targets, ir.StoreTarget{Kind: "local", Local: result.Local})
		}
		out.Results = append(out.Results, namedResults...)
		statements := append([]ir.Statement(nil), prelude...)
		if len(values) == 1 {
			statements = append(statements, ir.Statement{Kind: ir.StmtStoreResults, Expr: values[0], Targets: targets})
		} else {
			statements = append(statements, ir.Statement{Kind: ir.StmtStoreValues, Values: values, Targets: targets})
		}
		statements = append(statements,
			out,
			ir.Statement{Kind: ir.StmtLabel, Label: l.newLabel("return.after")},
		)
		return statements, true
	}
	out.Results = append(out.Results, values...)
	prelude = append(prelude, out, ir.Statement{Kind: ir.StmtLabel, Label: l.newLabel("return.after")})
	return prelude, true
}

func (l *lowerer) lowerRangeFunctionReturn(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	ctx := scope.rangeReturn
	if ctx == nil {
		l.add("hirgen.range.func.return", "range function return context is missing", stmt.Span)
		return nil, false
	}
	var statements []ir.Statement
	if len(stmt.Results) == 0 {
		if len(ctx.namedResultLocals) != 0 {
			if !l.validateRangeFunctionBareReturnNamedResults(scope, ctx, stmt.Span) {
				return nil, false
			}
		} else if ctx.expectedReturns != 0 {
			l.add("hirgen.return.bare", "bare return requires named results", stmt.Span)
			return nil, false
		}
	} else {
		if ctx.expectedReturns == 0 {
			l.add("hirgen.return.results", "return result count does not match function results", stmt.Span)
			return nil, false
		}
		prelude, values, ok := l.lowerReturnValueList(stmt.Results, l.typeStringsFromRefs(ctx.resultTypes), scope)
		if !ok {
			return nil, false
		}
		if hirResultsCount(values) != ctx.expectedReturns {
			l.add("hirgen.return.results", "return result count does not match function results", stmt.Span)
			return nil, false
		}
		statements = append(statements, prelude...)
		if len(values) == 1 {
			statements = append(statements, ir.Statement{Kind: ir.StmtStoreResults, Expr: values[0], Targets: ctx.resultTargets})
		} else {
			statements = append(statements, ir.Statement{Kind: ir.StmtStoreValues, Values: values, Targets: ctx.resultTargets})
		}
	}
	trueValue := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Bool"), Value: constant.Scalar(`true`)}
	statements = append(statements,
		ir.Statement{Kind: ir.StmtStoreUpvalue, Upvalue: ctx.returnFlagUpvalue, Expr: trueValue},
		ir.Statement{Kind: ir.StmtJump, Label: ctx.stopLabel},
		ir.Statement{Kind: ir.StmtLabel, Label: l.newLabel("return.after")},
	)
	return statements, true
}

func (l *lowerer) validateRangeFunctionBareReturnNamedResults(scope *funcScope, ctx *rangeFunctionReturnContext, span source.Span) bool {
	if scope == nil || ctx == nil {
		return true
	}
	for name := range ctx.namedResultLocals {
		for current := scope; current != nil && current.function == scope.function; current = current.outer {
			if _, ok := current.locals[name]; ok {
				l.add("hirgen.return.named_shadow", fmt.Sprintf("named result %s is shadowed at bare return", name), span)
				return false
			}
		}
	}
	return true
}

func (l *lowerer) lowerReturnExpressions(results []ast.Expression, resultTypes []string, scope *funcScope) ([]ir.Expression, bool) {
	values := make([]ir.Expression, 0, len(results))
	for i, result := range results {
		targetType := ""
		if len(results) == len(resultTypes) && i < len(resultTypes) {
			targetType = resultTypes[i]
		}
		expr, ok := l.lowerExpressionInType(result, targetType, scope)
		if !ok {
			return nil, false
		}
		values = append(values, expr)
	}
	return values, true
}

func (l *lowerer) lowerReturnValueList(results []ast.Expression, resultTypes []string, scope *funcScope) ([]ir.Statement, []ir.Expression, bool) {
	if len(results) == 1 && len(resultTypes) > 1 && results[0].Kind == ast.ExprCall {
		value, ok := l.lowerMultiResultValue(results[0], len(resultTypes), scope, "hirgen.return.results")
		if !ok {
			return nil, nil, false
		}
		if !l.validateMultiResultTargets(value, resultTypes, "hirgen.return.type", results[0]) {
			return nil, nil, false
		}
		if !multiResultNeedsNormalization(value.types, resultTypes) {
			return nil, []ir.Expression{value.expr}, true
		}
		binding, ok := l.bindMultiResult(value, resultTypes, scope, "hirgen.return.type", results[0])
		if !ok {
			return nil, nil, false
		}
		return []ir.Statement{binding.capture()}, binding.values, true
	}
	values, ok := l.lowerReturnExpressions(results, resultTypes, scope)
	if !ok {
		return nil, nil, false
	}
	for _, value := range values {
		if hirResultCount(value) != 1 {
			l.add("hirgen.return.results", "multi-valued expression must be the only return value", results[0].Span)
			return nil, nil, false
		}
	}
	return nil, values, true
}

func (l *lowerer) validateBareReturnNamedResults(scope *funcScope, span source.Span) bool {
	if scope == nil {
		return true
	}
	for name, resultLocal := range scope.namedResultLocals {
		local, _, ok := l.lookupLocal(name, scope)
		if !ok || local != resultLocal {
			l.add("hirgen.return.named_shadow", fmt.Sprintf("named result %s is shadowed at bare return", name), span)
			return false
		}
	}
	return true
}
