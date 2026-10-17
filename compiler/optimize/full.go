package optimize

import (
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
)

func foldPureBooleanBranches(body []hir.Statement) ([]hir.Statement, bool) {
	out := append([]hir.Statement(nil), body...)
	changed := false
	for index := range out {
		if out[index].Kind != hir.StmtJumpIf {
			continue
		}
		folded, ok := foldPureBoolean(out[index].Expr)
		if ok {
			out[index].Expr = folded
			changed = true
		}
	}
	return out, changed
}

func foldPureBoolean(expression hir.Expression) (hir.Expression, bool) {
	if expression.Kind == hir.ExprUnary && expression.Operand != nil && expression.Operator == "!" {
		operand, changed := foldPureBoolean(*expression.Operand)
		if changed {
			expression.Operand = &operand
		}
		if value, ok := booleanLiteral(operand); ok {
			return booleanExpression(expression, !value), true
		}
		return expression, changed
	}
	if expression.Kind != hir.ExprBinary || expression.Left == nil || expression.Right == nil {
		return expression, false
	}
	left, leftChanged := foldPureBoolean(*expression.Left)
	right, rightChanged := foldPureBoolean(*expression.Right)
	expression.Left, expression.Right = &left, &right
	leftValue, leftOK := booleanLiteral(left)
	rightValue, rightOK := booleanLiteral(right)
	if !leftOK || !rightOK {
		return expression, leftChanged || rightChanged
	}
	var result bool
	switch expression.Operator {
	case "&&":
		result = leftValue && rightValue
	case "||":
		result = leftValue || rightValue
	case "==":
		result = leftValue == rightValue
	case "!=":
		result = leftValue != rightValue
	default:
		return expression, leftChanged || rightChanged
	}
	return booleanExpression(expression, result), true
}

func booleanLiteral(expression hir.Expression) (bool, bool) {
	if expression.Kind != hir.ExprLiteral {
		return false, false
	}
	if expression.Value == nil || expression.Value.Kind() != constant.Boolean {
		return false, false
	}
	return expression.Value.Text == "true", true
}

func booleanExpression(original hir.Expression, value bool) hir.Expression {
	raw := constant.Scalar(`false`)
	if value {
		raw = constant.Scalar(`true`)
	}
	return hir.Expression{Kind: hir.ExprLiteral, Type: original.Type, Untyped: original.Untyped, Value: raw}
}

func propagateAdjacentBooleanLocals(body []hir.Statement) ([]hir.Statement, bool) {
	// This pass intentionally handles the immediate branch before the CFG
	// pass's address/capture protection is applied: the branch reads the
	// already stored boolean, while the later escape keeps the local storage
	// available for the address or closure that follows it.
	out := append([]hir.Statement(nil), body...)
	var local string
	var value hir.Expression
	changed := false
	for index := range out {
		statement := &out[index]
		if statement.Kind == hir.StmtJumpIf && statement.Expr.Kind == hir.ExprLocal && statement.Expr.Local == local {
			statement.Expr = value
			changed = true
		}
		local = ""
		value = hir.Expression{}
		if statement.Expr.Kind != hir.ExprLiteral {
			continue
		}
		var storedLocal string
		switch statement.Kind {
		case hir.StmtStoreLocal:
			if !statement.Rebind {
				storedLocal = statement.Local
			}
		case hir.StmtStoreResults:
			// Source assignments use StoreResults even for a single literal.
			// Keep the store (including Rebind); only the adjacent read is replaced.
			if len(statement.Targets) == 1 && statement.Targets[0].Kind == "local" {
				storedLocal = statement.Targets[0].Local
			}
		}
		if storedLocal == "" {
			continue
		}
		if statement.Expr.Value == nil || statement.Expr.Value.Kind() != constant.Boolean {
			continue
		}
		local = storedLocal
		value = statement.Expr
	}
	return out, changed
}

func removeDeadPureLocalStores(body []hir.Statement, resultLocals []string) ([]hir.Statement, bool) {
	out := append([]hir.Statement(nil), body...)
	changed := false
	for {
		next, removed := removeDeadPureLocalStoresOnce(out, resultLocals)
		out = next
		changed = changed || removed
		if !removed {
			return out, changed
		}
	}
}

func statementSuccessors(body []hir.Statement, labels map[string]int, index int) []int {
	statement := body[index]
	switch statement.Kind {
	case hir.StmtTypeDispatch:
		out := []int{labels[statement.Label]}
		for _, match := range statement.TypeCases {
			out = append(out, labels[match.Label])
		}
		return out
	case hir.StmtJump:
		return []int{labels[statement.Label]}
	case hir.StmtJumpIf:
		out := []int{labels[statement.Label]}
		if index+1 < len(body) {
			out = append(out, index+1)
		}
		return out
	case hir.StmtReturn, hir.StmtTailCallDirect, hir.StmtPanic:
		return nil
	default:
		if index+1 < len(body) {
			return []int{index + 1}
		}
		return nil
	}
}

func collectStatementProtectedLocals(statement hir.Statement, protected map[string]struct{}) {
	for _, expression := range []hir.Expression{statement.Expr, statement.Object, statement.Index} {
		collectExpressionProtectedLocals(expression, protected)
	}
	for _, expressions := range [][]hir.Expression{statement.Results, statement.Values, statement.Args} {
		for _, expression := range expressions {
			collectExpressionProtectedLocals(expression, protected)
		}
	}
}

func collectExpressionProtectedLocals(expression hir.Expression, protected map[string]struct{}) {
	// Let bindings are expression-local writes rather than CFG statements.
	// Exclude their locals from statement data-flow substitution.
	if expression.Kind == hir.ExprLet && expression.Local != "" {
		protected[expression.Local] = struct{}{}
	}
	if expression.Kind == hir.ExprLetResults {
		for _, local := range expression.Locals {
			if local != "" {
				protected[local] = struct{}{}
			}
		}
	}
	if expression.Kind == hir.ExprAddressOf && expression.Local != "" {
		protected[expression.Local] = struct{}{}
	}
	if expression.Kind == hir.ExprAddressOf {
		for _, segment := range expression.Path {
			if segment.Local != "" {
				protected[segment.Local] = struct{}{}
			}
		}
	}
	for _, capture := range expression.Captures {
		if capture.Local != "" {
			protected[capture.Local] = struct{}{}
		}
	}
	for _, child := range []*hir.Expression{expression.Left, expression.Right, expression.Operand, expression.Bind, expression.Body, expression.Size, expression.Index, expression.Start, expression.End, expression.Max} {
		if child != nil {
			collectExpressionProtectedLocals(*child, protected)
		}
	}
	for _, expressions := range [][]hir.Expression{expression.Args, expression.Elements} {
		for _, child := range expressions {
			collectExpressionProtectedLocals(child, protected)
		}
	}
	for _, entry := range expression.Entries {
		collectExpressionProtectedLocals(entry.Key, protected)
		collectExpressionProtectedLocals(entry.Value, protected)
	}
	for _, field := range expression.Fields {
		collectExpressionProtectedLocals(field.Value, protected)
	}
}

func isPureValueExpression(expression hir.Expression) bool {
	switch expression.Kind {
	case hir.ExprLiteral, hir.ExprConst, hir.ExprLocal, hir.ExprUpvalue, hir.ExprGlobal:
		return true
	default:
		return false
	}
}

func collectStatementLocalReads(statement hir.Statement, add func(string)) {
	if statement.Kind == hir.StmtTypeDispatch {
		add(statement.Local)
	}
	for _, selected := range statement.SelectCases {
		add(selected.Channel)
		if selected.Send != "" {
			add(selected.Send)
		}
	}
	collectExpressionLocalReads(statement.Expr, add)
	collectExpressionLocalReads(statement.Object, add)
	collectExpressionLocalReads(statement.Index, add)
	for _, expressions := range [][]hir.Expression{statement.Results, statement.Values, statement.Args} {
		for _, expression := range expressions {
			collectExpressionLocalReads(expression, add)
		}
	}
	for _, target := range statement.Targets {
		if target.Local != "" && target.Kind != "local" {
			add(target.Local)
		}
	}
}

func collectExpressionLocalReads(expression hir.Expression, add func(string)) {
	if expression.Local != "" {
		add(expression.Local)
	}
	for _, local := range expression.Locals {
		if local != "" {
			add(local)
		}
	}
	for _, capture := range expression.Captures {
		if capture.Local != "" {
			add(capture.Local)
		}
	}
	for _, segment := range expression.Path {
		if segment.Local != "" {
			add(segment.Local)
		}
	}
	for _, child := range []*hir.Expression{expression.Left, expression.Right, expression.Operand, expression.Bind, expression.Body, expression.Size, expression.Index, expression.Start, expression.End, expression.Max} {
		if child != nil {
			collectExpressionLocalReads(*child, add)
		}
	}
	for _, expressions := range [][]hir.Expression{expression.Args, expression.Elements} {
		for _, child := range expressions {
			collectExpressionLocalReads(child, add)
		}
	}
	for _, entry := range expression.Entries {
		collectExpressionLocalReads(entry.Key, add)
		collectExpressionLocalReads(entry.Value, add)
	}
	for _, field := range expression.Fields {
		collectExpressionLocalReads(field.Value, add)
	}
}
