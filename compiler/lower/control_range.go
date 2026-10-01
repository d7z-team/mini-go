package lower

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) lowerRangeWithLabel(stmt ast.Statement, scope *funcScope, userLabel string) ([]ir.Statement, bool) {
	if stmt.Range == nil {
		l.add("hirgen.range.expr.missing", "range statement requires an expression", stmt.Span)
		return nil, false
	}
	object, ok := l.lowerExpression(*stmt.Range, scope)
	if !ok {
		return nil, false
	}
	switch l.rangeKind(*stmt.Range, scope) {
	case "map":
		return l.lowerMapRange(stmt, scope, object, userLabel)
	case "chan":
		return l.lowerChanRange(stmt, scope, object, userLabel)
	case "integer":
		return l.lowerIntegerRange(stmt, scope, object, userLabel)
	case "function":
		return l.lowerFunctionRange(stmt, scope, object, userLabel)
	case "invalid":
		l.add("hirgen.range.type", "range expression must be an array, slice, string, map, channel, integer, or iterator function", stmt.Range.Span)
		return nil, false
	}
	objectType := l.expressionType(*stmt.Range, scope)
	elementContainer := objectType
	if arrayType, ok := l.pointerArrayType(objectType); ok {
		elementContainer = arrayType
	}
	rangeValueType := l.indexElementType(elementContainer)
	if l.isStringType(objectType) {
		rangeValueType = "Int32"
	}
	rangeScope := l.childScope(scope)
	if !l.declareRangeShortTargets(stmt, "Int", rangeValueType, rangeScope) {
		return nil, false
	}
	objectLocal := l.newSyntheticLocal(rangeScope, "range.object", objectType)
	indexLocal := l.newSyntheticLocal(rangeScope, "range.index", "Int")
	lenLocal := l.newSyntheticLocal(rangeScope, "range.len", "Int")
	objectRef := ir.Expression{Kind: ir.ExprLocal, Local: objectLocal, Type: l.hirType(objectType)}
	indexRef := ir.Expression{Kind: ir.ExprLocal, Local: indexLocal, Type: l.hirType("Int")}
	lenRef := ir.Expression{Kind: ir.ExprLocal, Local: lenLocal, Type: l.hirType("Int")}
	zero := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: constant.Scalar(`0`)}
	one := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: constant.Scalar(`1`)}
	length := ir.Expression{Kind: ir.ExprLen, Operand: &objectRef}
	evaluateObject := true
	if n, _, array := l.arrayTypeInfo(elementContainer); array {
		length = ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: constant.Scalar(strconv.FormatInt(n, 10))}
		if (stmt.Value == nil || stmt.Value.Kind == ast.ExprIdent && stmt.Value.Name == "_") && !check.ContainsNonConstantLenOperand(*stmt.Range) {
			evaluateObject = false
		}
	}
	condLabel := l.newLabel("range.cond")
	bodyLabel := l.newLabel("range.body")
	postLabel := l.newLabel("range.post")
	endLabel := l.newLabel("range.end")
	out := []ir.Statement{
		{Kind: ir.StmtStoreLocal, Local: objectLocal, Expr: object},
		{Kind: ir.StmtStoreLocal, Local: indexLocal, Expr: zero},
		{Kind: ir.StmtStoreLocal, Local: lenLocal, Expr: length},
		{Kind: ir.StmtLabel, Label: condLabel},
		{Kind: ir.StmtJumpIf, Label: bodyLabel, Expr: ir.Expression{
			Kind:     ir.ExprBinary,
			Operator: "<",
			Left:     &indexRef,
			Right:    &lenRef,
		}},
		{Kind: ir.StmtJump, Label: endLabel},
		{Kind: ir.StmtLabel, Label: bodyLabel},
	}
	if !evaluateObject {
		out = out[1:]
	}
	values := []ir.Expression{indexRef}
	if stmt.Value != nil {
		valueType := l.indexElementType(elementContainer)
		valueKind := ir.ExprLoadIndex
		if l.isStringType(objectType) {
			valueType = "Int32"
			valueKind = ir.ExprStringRuneAt
		}
		value := ir.Expression{Kind: valueKind, Type: l.hirType(valueType), Operand: &objectRef, Index: &indexRef}
		values = append(values, value)
	}
	stores, ok := l.lowerRangeTargets(stmt, values, rangeScope)
	if !ok {
		return nil, false
	}
	out = append(out, stores...)
	l.pushBranchWithUserLabel(userLabel, endLabel, postLabel)
	body, ok := l.lowerBlock(stmt.Body, rangeScope)
	l.popBranch()
	if !ok {
		return nil, false
	}
	out = append(out, body...)
	postExpression := ir.Expression{
		Kind:     ir.ExprBinary,
		Operator: "+",
		Left:     &indexRef,
		Right:    &one,
	}
	if l.isStringType(objectType) {
		postExpression = ir.Expression{Kind: ir.ExprStringNextRuneIndex, Operand: &objectRef, Index: &indexRef}
	}
	out = append(out,
		ir.Statement{Kind: ir.StmtLabel, Label: postLabel},
		ir.Statement{Kind: ir.StmtStoreLocal, Local: indexLocal, Expr: postExpression},
		ir.Statement{Kind: ir.StmtJump, Label: condLabel},
		ir.Statement{Kind: ir.StmtLabel, Label: endLabel},
	)
	return out, true
}

func (l *lowerer) lowerIntegerRange(stmt ast.Statement, scope *funcScope, object ir.Expression, userLabel string) ([]ir.Statement, bool) {
	if stmt.Value != nil {
		l.add("hirgen.range.integer.targets", "integer range accepts at most one iteration variable", stmt.Value.Span)
		return nil, false
	}
	objectType := l.expressionType(*stmt.Range, scope)
	rangeScope := l.childScope(scope)
	if !l.declareRangeShortTargets(stmt, "Int", "", rangeScope) {
		return nil, false
	}
	objectLocal := l.newSyntheticLocal(rangeScope, "range.integer.limit", "Int")
	indexLocal := l.newSyntheticLocal(rangeScope, "range.integer.index", "Int")
	objectRef := ir.Expression{Kind: ir.ExprLocal, Local: objectLocal, Type: l.hirType("Int")}
	indexRef := ir.Expression{Kind: ir.ExprLocal, Local: indexLocal, Type: l.hirType("Int")}
	zero := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: constant.Scalar(`0`)}
	one := ir.Expression{Kind: ir.ExprLiteral, Type: l.hirType("Int"), Value: constant.Scalar(`1`)}
	limit := object
	if strings.TrimSpace(objectType) != "Int" {
		limit = ir.Expression{Kind: ir.ExprConvert, Type: l.hirType("Int"), Operand: &object}
	}
	condLabel := l.newLabel("range.integer.cond")
	bodyLabel := l.newLabel("range.integer.body")
	postLabel := l.newLabel("range.integer.post")
	endLabel := l.newLabel("range.integer.end")
	out := []ir.Statement{
		{Kind: ir.StmtStoreLocal, Local: objectLocal, Expr: limit},
		{Kind: ir.StmtStoreLocal, Local: indexLocal, Expr: zero},
		{Kind: ir.StmtLabel, Label: condLabel},
		{Kind: ir.StmtJumpIf, Label: bodyLabel, Expr: ir.Expression{
			Kind:     ir.ExprBinary,
			Operator: "<",
			Left:     &indexRef,
			Right:    &objectRef,
		}},
		{Kind: ir.StmtJump, Label: endLabel},
		{Kind: ir.StmtLabel, Label: bodyLabel},
	}
	if stmt.Key != nil {
		stores, ok := l.lowerRangeTargets(stmt, []ir.Expression{indexRef}, rangeScope)
		if !ok {
			return nil, false
		}
		out = append(out, stores...)
	}
	l.pushBranchWithUserLabel(userLabel, endLabel, postLabel)
	body, ok := l.lowerBlock(stmt.Body, rangeScope)
	l.popBranch()
	if !ok {
		return nil, false
	}
	out = append(out, body...)
	out = append(out,
		ir.Statement{Kind: ir.StmtLabel, Label: postLabel},
		ir.Statement{Kind: ir.StmtStoreLocal, Local: indexLocal, Expr: ir.Expression{
			Kind:     ir.ExprBinary,
			Operator: "+",
			Left:     &indexRef,
			Right:    &one,
		}},
		ir.Statement{Kind: ir.StmtJump, Label: condLabel},
		ir.Statement{Kind: ir.StmtLabel, Label: endLabel},
	)
	return out, true
}

func (l *lowerer) lowerMapRange(stmt ast.Statement, scope *funcScope, object ir.Expression, userLabel string) ([]ir.Statement, bool) {
	objectType := l.expressionType(*stmt.Range, scope)
	keyType, valueType, _ := l.mapKeyValueTypes(objectType)
	rangeScope := l.childScope(scope)
	if !l.declareRangeShortTargets(stmt, keyType, valueType, rangeScope) {
		return nil, false
	}
	iterator := l.newSyntheticLocal(rangeScope, "range.map", objectType)
	keyLocal := l.newSyntheticLocal(rangeScope, "range.key", keyType)
	valueLocal := l.newSyntheticLocal(rangeScope, "range.value", valueType)
	okLocal := l.newSyntheticLocal(rangeScope, "range.ok", "Bool")
	condLabel := l.newLabel("range.map.cond")
	bodyLabel := l.newLabel("range.map.body")
	endLabel := l.newLabel("range.map.end")
	out := []ir.Statement{
		{Kind: ir.StmtMapIterInit, Local: iterator, Expr: object},
		{Kind: ir.StmtLabel, Label: condLabel},
		{
			Kind: ir.StmtStoreResults, Expr: ir.Expression{Kind: ir.ExprMapIterNext, Local: iterator, ResultCount: 3},
			Targets: []ir.StoreTarget{{Kind: "local", Local: keyLocal}, {Kind: "local", Local: valueLocal}, {Kind: "local", Local: okLocal}},
		},
		{Kind: ir.StmtJumpIf, Label: bodyLabel, Expr: ir.Expression{Kind: ir.ExprLocal, Local: okLocal, Type: l.hirType("Bool")}},
		{Kind: ir.StmtJump, Label: endLabel},
		{Kind: ir.StmtLabel, Label: bodyLabel},
	}
	values := []ir.Expression{
		{Kind: ir.ExprLocal, Local: keyLocal, Type: l.hirType(keyType)},
		{Kind: ir.ExprLocal, Local: valueLocal, Type: l.hirType(valueType)},
	}
	stores, ok := l.lowerRangeTargets(stmt, values, rangeScope)
	if !ok {
		return nil, false
	}
	out = append(out, stores...)
	l.pushBranchWithUserLabel(userLabel, endLabel, condLabel)
	body, ok := l.lowerBlock(stmt.Body, rangeScope)
	l.popBranch()
	if !ok {
		return nil, false
	}
	out = append(out, body...)
	out = append(out, ir.Statement{Kind: ir.StmtJump, Label: condLabel},
		ir.Statement{Kind: ir.StmtLabel, Label: endLabel},
		ir.Statement{Kind: ir.StmtMapIterClose, Local: iterator})
	return out, true
}

func (l *lowerer) lowerChanRange(stmt ast.Statement, scope *funcScope, object ir.Expression, userLabel string) ([]ir.Statement, bool) {
	if stmt.Value != nil {
		l.add("hirgen.range.chan.targets", "channel range accepts at most one iteration variable", stmt.Value.Span)
		return nil, false
	}
	objectType := l.expressionType(*stmt.Range, scope)
	valueType := l.channelElementType(objectType)
	rangeScope := l.childScope(scope)
	if !l.declareRangeShortTargets(stmt, valueType, "", rangeScope) {
		return nil, false
	}
	objectLocal := l.newSyntheticLocal(rangeScope, "range.chan", objectType)
	valueLocal := l.newSyntheticLocal(rangeScope, "range.chan.value", valueType)
	okLocal := l.newSyntheticLocal(rangeScope, "range.chan.ok", "Bool")
	objectRef := ir.Expression{Kind: ir.ExprLocal, Local: objectLocal, Type: l.hirType(objectType)}
	valueRef := ir.Expression{Kind: ir.ExprLocal, Local: valueLocal, Type: l.hirType(valueType)}
	okRef := ir.Expression{Kind: ir.ExprLocal, Local: okLocal, Type: l.hirType("Bool")}
	condLabel := l.newLabel("range.chan.cond")
	bodyLabel := l.newLabel("range.chan.body")
	postLabel := l.newLabel("range.chan.post")
	endLabel := l.newLabel("range.chan.end")
	out := []ir.Statement{
		{Kind: ir.StmtStoreLocal, Local: objectLocal, Expr: object},
		{Kind: ir.StmtLabel, Label: condLabel},
		{Kind: ir.StmtStoreResults, Expr: ir.Expression{Kind: ir.ExprChanRecvOK, Operand: &objectRef}, Targets: []ir.StoreTarget{
			{Kind: "local", Local: valueLocal},
			{Kind: "local", Local: okLocal},
		}},
		{Kind: ir.StmtJumpIf, Expr: okRef, Label: bodyLabel},
		{Kind: ir.StmtJump, Label: endLabel},
		{Kind: ir.StmtLabel, Label: bodyLabel},
	}
	target := stmt.Key
	if stmt.Value != nil {
		target = stmt.Value
	}
	if target != nil {
		stores, ok := l.lowerRangeTargets(stmt, []ir.Expression{valueRef}, rangeScope)
		if !ok {
			return nil, false
		}
		out = append(out, stores...)
	}
	l.pushBranchWithUserLabel(userLabel, endLabel, postLabel)
	body, ok := l.lowerBlock(stmt.Body, rangeScope)
	l.popBranch()
	if !ok {
		return nil, false
	}
	out = append(out, body...)
	out = append(out,
		ir.Statement{Kind: ir.StmtLabel, Label: postLabel},
		ir.Statement{Kind: ir.StmtJump, Label: condLabel},
		ir.Statement{Kind: ir.StmtLabel, Label: endLabel},
	)
	return out, true
}

// Iteration values are obtained before the assignment operands. In particular,
// assigning the key must not affect evaluation of the value destination.
func (l *lowerer) lowerRangeTargets(stmt ast.Statement, values []ir.Expression, scope *funcScope) ([]ir.Statement, bool) {
	var targets []ast.Expression
	if stmt.Key != nil && len(values) != 0 {
		targets = append(targets, *stmt.Key)
	}
	if stmt.Value != nil && len(values) > 1 {
		targets = append(targets, *stmt.Value)
	}
	var out []ir.Statement
	refs := make([]ir.Expression, len(targets))
	for i, target := range targets {
		if target.Kind == ast.ExprIdent && target.Name == "_" {
			refs[i] = values[i]
			continue
		}
		local := l.newSyntheticLocal(scope, "range.value", l.typeRefString(values[i].Type))
		out = append(out, ir.Statement{Kind: ir.StmtStoreLocal, Local: local, Expr: values[i]})
		refs[i] = ir.Expression{Kind: ir.ExprLocal, Local: local, Type: values[i].Type}
	}
	plans, ok := l.planLValues(targets, scope)
	if !ok {
		return nil, false
	}
	out = appendLValuePreludes(out, plans)
	for i, plan := range plans {
		if plan.kind == lvalueDiscard {
			continue
		}
		if relation, structured := l.assignmentRelationRef(refs[i].Type, plan.typ); !structured || !relation.OK {
			l.add("hirgen.assign.type", fmt.Sprintf("cannot assign %s to %s", l.typeRefString(refs[i].Type), plan.typ), targets[i].Span)
			return nil, false
		}
		store := plan.store(refs[i])
		store.Rebind = stmt.Op == ":=" && store.Kind == ir.StmtStoreLocal
		out = append(out, store)
	}
	return out, true
}

func (l *lowerer) declareRangeShortTargets(stmt ast.Statement, keyType, valueType string, scope *funcScope) bool {
	if stmt.Op != ":=" {
		return true
	}
	if scope == nil || scope.function == nil {
		l.add("hirgen.range.short.scope", "range short variable declaration requires function scope", stmt.Span)
		return false
	}
	targets := []struct {
		expr *ast.Expression
		typ  string
	}{
		{expr: stmt.Key, typ: keyType},
		{expr: stmt.Value, typ: valueType},
	}
	seen := map[string]struct{}{}
	newName := false
	for _, target := range targets {
		if target.expr == nil {
			continue
		}
		if target.expr.Kind != ast.ExprIdent {
			l.add("hirgen.range.short.target", "range short variable declaration requires identifier targets", target.expr.Span)
			return false
		}
		name := strings.TrimSpace(target.expr.Name)
		if name == "" || name == "_" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			l.add("hirgen.range.short.duplicate", "range short variable declaration has duplicate target name", target.expr.Span)
			return false
		}
		seen[name] = struct{}{}
		if _, exists := scope.constants[name]; exists {
			l.add("hirgen.range.short.constant", "range short variable declaration cannot redeclare a constant", target.expr.Span)
			return false
		}
		if _, exists := l.currentLocal(name, scope); exists {
			continue
		}
		typ := strings.TrimSpace(target.typ)
		if typ == "" {
			typ = "Any"
		}
		id := l.newLocalID(scope.function, name)
		scope.function.Locals = append(scope.function.Locals, ir.Local{
			ID: id, Name: name, Type: l.hirType(typ), Scope: scope.debugScope, Declaration: hirLocationPtr(target.expr.Span),
		})
		scope.locals[name] = id
		scope.localTypes[name] = l.hirType(typ)
		newName = true
	}
	if !newName {
		l.add("hirgen.range.short.no_new", "range short variable declaration requires at least one new variable", stmt.Span)
		return false
	}
	return true
}
