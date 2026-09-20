package optimize

import (
	"encoding/json"

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
	var value bool
	if json.Unmarshal(expression.Value, &value) != nil {
		return false, false
	}
	return value, true
}

func booleanExpression(original hir.Expression, value bool) hir.Expression {
	raw := json.RawMessage(`false`)
	if value {
		raw = json.RawMessage(`true`)
	}
	return hir.Expression{Kind: hir.ExprLiteral, Type: original.Type, Untyped: original.Untyped, Value: raw}
}

func propagateAdjacentBooleanLocals(body []hir.Statement) ([]hir.Statement, bool) {
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
		var boolean bool
		if json.Unmarshal(statement.Expr.Value, &boolean) != nil {
			continue
		}
		local = storedLocal
		value = statement.Expr
	}
	return out, changed
}

func removeDeadPureLocalStores(body []hir.Statement) ([]hir.Statement, bool) {
	out := append([]hir.Statement(nil), body...)
	changed := false
	for {
		next, removed := removeDeadPureLocalStoresOnce(out)
		out = next
		changed = changed || removed
		if !removed {
			return out, changed
		}
	}
}

func removeDeadPureLocalStoresOnce(body []hir.Statement) ([]hir.Statement, bool) {
	labels := make(map[string]int)
	protected := make(map[string]struct{})
	for index, statement := range body {
		if statement.Kind == hir.StmtLabel {
			labels[statement.Label] = index
		}
		collectStatementProtectedLocals(statement, protected)
	}
	liveIn := make([]map[string]struct{}, len(body))
	liveOut := make([]map[string]struct{}, len(body))
	for changed := true; changed; {
		changed = false
		for index := len(body) - 1; index >= 0; index-- {
			out := make(map[string]struct{})
			for _, successor := range statementSuccessors(body, labels, index) {
				for local := range liveIn[successor] {
					out[local] = struct{}{}
				}
			}
			in := cloneLocalSet(out)
			statement := body[index]
			if statement.Kind == hir.StmtStoreLocal {
				delete(in, statement.Local)
			}
			if statement.Kind == hir.StmtStoreResults || statement.Kind == hir.StmtStoreValues {
				for _, target := range statement.Targets {
					if target.Kind == "local" {
						delete(in, target.Local)
					}
				}
			}
			collectStatementLocalReads(statement, in)
			if !equalLocalSets(liveOut[index], out) || !equalLocalSets(liveIn[index], in) {
				liveOut[index], liveIn[index] = out, in
				changed = true
			}
		}
	}
	out := make([]hir.Statement, 0, len(body))
	var pending []hir.Location
	changed := false
	for index, statement := range body {
		_, live := liveOut[index][statement.Local]
		_, escaped := protected[statement.Local]
		if statement.Kind == hir.StmtStoreLocal && !statement.Rebind && !live && !escaped && isPureValueExpression(statement.Expr) {
			pending = appendUniqueLocations(pending, statement.SourcePoints)
			changed = true
			continue
		}
		if len(pending) != 0 {
			statement.SourcePoints = prependUniqueLocations(pending, statement.SourcePoints)
			pending = nil
		}
		out = append(out, statement)
	}
	if len(pending) != 0 && len(out) != 0 {
		out[len(out)-1].SourcePoints = appendUniqueLocations(out[len(out)-1].SourcePoints, pending)
	}
	return out, changed
}

func statementSuccessors(body []hir.Statement, labels map[string]int, index int) []int {
	statement := body[index]
	switch statement.Kind {
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

func cloneLocalSet(input map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(input))
	for local := range input {
		out[local] = struct{}{}
	}
	return out
}

func equalLocalSets(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for local := range left {
		if _, ok := right[local]; !ok {
			return false
		}
	}
	return true
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
	if expression.Kind == hir.ExprAddressOf && expression.Local != "" {
		protected[expression.Local] = struct{}{}
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
	case hir.ExprLiteral, hir.ExprConst, hir.ExprZero, hir.ExprLocal, hir.ExprUpvalue, hir.ExprGlobal:
		return true
	default:
		return false
	}
}

func collectStatementLocalReads(statement hir.Statement, used map[string]struct{}) {
	for _, selected := range statement.SelectCases {
		used[selected.Channel] = struct{}{}
		if selected.Send != "" {
			used[selected.Send] = struct{}{}
		}
	}
	collectExpressionLocalReads(statement.Expr, used)
	collectExpressionLocalReads(statement.Object, used)
	collectExpressionLocalReads(statement.Index, used)
	for _, expressions := range [][]hir.Expression{statement.Results, statement.Values, statement.Args} {
		for _, expression := range expressions {
			collectExpressionLocalReads(expression, used)
		}
	}
	for _, target := range statement.Targets {
		if target.Local != "" && target.Kind != "local" {
			used[target.Local] = struct{}{}
		}
	}
}

func collectExpressionLocalReads(expression hir.Expression, used map[string]struct{}) {
	if expression.Local != "" {
		used[expression.Local] = struct{}{}
	}
	for _, local := range expression.Locals {
		if local != "" {
			used[local] = struct{}{}
		}
	}
	for _, capture := range expression.Captures {
		if capture.Local != "" {
			used[capture.Local] = struct{}{}
		}
	}
	for _, segment := range expression.Path {
		if segment.Local != "" {
			used[segment.Local] = struct{}{}
		}
	}
	for _, child := range []*hir.Expression{expression.Left, expression.Right, expression.Operand, expression.Bind, expression.Body, expression.Size, expression.Index, expression.Start, expression.End, expression.Max} {
		if child != nil {
			collectExpressionLocalReads(*child, used)
		}
	}
	for _, expressions := range [][]hir.Expression{expression.Args, expression.Elements} {
		for _, child := range expressions {
			collectExpressionLocalReads(child, used)
		}
	}
	for _, entry := range expression.Entries {
		collectExpressionLocalReads(entry.Key, used)
		collectExpressionLocalReads(entry.Value, used)
	}
	for _, field := range expression.Fields {
		collectExpressionLocalReads(field.Value, used)
	}
}
