package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

type lvalueKind uint8

const (
	lvalueInvalid lvalueKind = iota
	lvalueDiscard
	lvalueSlot
	lvalueAddress
	lvalueMapIndex
)

type lvaluePlan struct {
	kind    lvalueKind
	typ     string
	prelude []ir.Statement
	slot    ir.StoreTarget
	address ir.Expression
	object  ir.Expression
	index   ir.Expression
}

func (l *lowerer) planLValues(targets []ast.Expression, scope *funcScope) ([]lvaluePlan, bool) {
	plans := make([]lvaluePlan, 0, len(targets))
	for _, target := range targets {
		plan, ok := l.planLValue(target, scope)
		if !ok {
			return nil, false
		}
		plans = append(plans, plan)
	}
	return plans, true
}

func (l *lowerer) planLValue(target ast.Expression, scope *funcScope) (lvaluePlan, bool) {
	if target.Kind == ast.ExprSelector && l.rejectAmbiguousSelector(target, scope, "hirgen.selector.ambiguous") {
		return lvaluePlan{}, false
	}
	targetType := l.assignmentTargetType(target, scope)
	_, category, structured := l.semanticValueFact(target)
	plan := lvaluePlan{typ: targetType}
	_, _, imported := l.importedVariable(target)
	if target.Kind == ast.ExprIdent && !imported {
		if strings.TrimSpace(target.Name) == "_" {
			plan.kind = lvalueDiscard
			return plan, true
		}
		store, ok := l.lowerStoreTarget(target, scope)
		if !ok {
			return lvaluePlan{}, false
		}
		plan.kind = lvalueSlot
		plan.slot = store
		return plan, true
	}
	if strings.TrimSpace(targetType) == "" {
		l.add("hirgen.assign.target.type", "assignment target has no type", target.Span)
		return lvaluePlan{}, false
	}
	if target.Kind == ast.ExprIndex && target.Operand != nil {
		objectType := l.expressionType(*target.Operand, scope)
		if l.isStringType(objectType) {
			l.add("hirgen.assign.index.string", "cannot assign to string index", target.Span)
			return lvaluePlan{}, false
		}
		if structured && category == check.ValueMapIndex {
			return l.planMapIndexLValue(target, targetType, scope)
		}
		if !structured {
			if _, _, ok := l.mapKeyValueTypes(objectType); ok {
				return l.planMapIndexLValue(target, targetType, scope)
			}
		}
	}
	if target.Kind == ast.ExprSelector && target.Operand != nil && l.hasMapIndexRoot(*target.Operand, scope) {
		l.add("hirgen.assign.member.map", "cannot assign to a field of a map index", target.Span)
		return lvaluePlan{}, false
	}
	if structured && !category.Assignable() {
		l.add("hirgen.assign.target", "assignment target is not addressable", target.Span)
		return lvaluePlan{}, false
	}
	address, ok := l.lowerAddressTargetFor(target, scope, true)
	if !ok {
		return lvaluePlan{}, false
	}
	pointerType := "Ptr<" + targetType + ">"
	address.expr.Type = l.hirType(pointerType)
	plan.kind = lvalueAddress
	plan.prelude = make([]ir.Statement, 0, len(address.lets))
	for _, let := range address.lets {
		plan.prelude = append(plan.prelude, ir.Statement{Kind: ir.StmtStoreLocal, Local: let.local, Expr: let.value})
	}
	// Freeze operand values now, but validate the final address only when it is
	// read or written. A plain assignment evaluates its RHS before that access.
	plan.address = address.expr
	return plan, true
}

func (l *lowerer) planMapIndexLValue(target ast.Expression, targetType string, scope *funcScope) (lvaluePlan, bool) {
	if target.Operand == nil || target.Index == nil {
		l.add("hirgen.assign.index.missing", "index assignment requires object and index", target.Span)
		return lvaluePlan{}, false
	}
	objectType := l.expressionType(*target.Operand, scope)
	if !l.validateMapKeyComparable(objectType, target.Span) {
		return lvaluePlan{}, false
	}
	object, ok := l.lowerExpression(*target.Operand, scope)
	if !ok {
		return lvaluePlan{}, false
	}
	keyType := l.mapIndexKeyType(*target.Operand, scope)
	index, ok := l.lowerIndexExpression(*target.Index, keyType, scope)
	if !ok {
		return lvaluePlan{}, false
	}
	objectType = firstNonEmpty(l.typeRefString(object.Type), objectType)
	keyType = firstNonEmpty(l.typeRefString(index.Type), keyType)
	objectLocal := l.newSyntheticLocal(scope, "assign.map", objectType)
	indexLocal := l.newSyntheticLocal(scope, "assign.key", keyType)
	return lvaluePlan{
		kind: lvalueMapIndex,
		typ:  targetType,
		prelude: []ir.Statement{
			{Kind: ir.StmtStoreLocal, Local: objectLocal, Expr: object},
			{Kind: ir.StmtStoreLocal, Local: indexLocal, Expr: index},
		},
		object: ir.Expression{Kind: ir.ExprLocal, Local: objectLocal, Type: l.hirType(objectType)},
		index:  ir.Expression{Kind: ir.ExprLocal, Local: indexLocal, Type: l.hirType(keyType)},
	}, true
}

func (plan lvaluePlan) load(l *lowerer) (ir.Expression, bool) {
	switch plan.kind {
	case lvalueSlot:
		return loadStoreTarget(plan.slot)
	case lvalueAddress:
		return ir.Expression{Kind: ir.ExprLoadIndirect, Type: l.hirType(plan.typ), Operand: &plan.address}, true
	case lvalueMapIndex:
		return ir.Expression{Kind: ir.ExprLoadIndex, Type: l.hirType(plan.typ), Operand: &plan.object, Index: &plan.index}, true
	default:
		return ir.Expression{}, false
	}
}

func (plan lvaluePlan) store(value ir.Expression) ir.Statement {
	switch plan.kind {
	case lvalueSlot:
		return storeTargetStatement(plan.slot, value)
	case lvalueAddress:
		return ir.Statement{Kind: ir.StmtStoreIndirect, Object: plan.address, Expr: value}
	case lvalueMapIndex:
		return ir.Statement{Kind: ir.StmtStoreIndex, Object: plan.object, Index: plan.index, Expr: value}
	default:
		return ir.Statement{}
	}
}

func appendLValuePreludes(out []ir.Statement, plans []lvaluePlan) []ir.Statement {
	for _, plan := range plans {
		out = append(out, plan.prelude...)
	}
	return out
}

func lvalueStoreTargets(plans []lvaluePlan) ([]ir.StoreTarget, bool) {
	targets := make([]ir.StoreTarget, 0, len(plans))
	for _, plan := range plans {
		switch plan.kind {
		case lvalueDiscard:
			targets = append(targets, ir.StoreTarget{Kind: "discard"})
		case lvalueSlot:
			targets = append(targets, plan.slot)
		default:
			return nil, false
		}
	}
	return targets, true
}

func (l *lowerer) lvalueResultTemps(plans []lvaluePlan, scope *funcScope) ([]ir.StoreTarget, []ir.Expression) {
	targets := make([]ir.StoreTarget, 0, len(plans))
	refs := make([]ir.Expression, 0, len(plans))
	for _, plan := range plans {
		if plan.kind == lvalueDiscard {
			targets = append(targets, ir.StoreTarget{Kind: "discard"})
			refs = append(refs, ir.Expression{})
			continue
		}
		local := l.newSyntheticLocal(scope, "assign.result", plan.typ)
		targets = append(targets, ir.StoreTarget{Kind: "local", Local: local})
		refs = append(refs, ir.Expression{Kind: ir.ExprLocal, Local: local, Type: l.hirType(plan.typ)})
	}
	return targets, refs
}

func (l *lowerer) lowerAddressTarget(expr ast.Expression, scope *funcScope) (addressTarget, bool) {
	target, ok := l.lowerAddressTargetFor(expr, scope, false)
	if ok {
		target.expr.Type = l.hirType("Ptr<" + l.expressionType(expr, scope) + ">")
	}
	return target, ok
}

func (l *lowerer) lowerAddressTargetFor(expr ast.Expression, scope *funcScope, assignment bool) (addressTarget, bool) {
	if modulePath, name, ok := l.importedVariable(expr); ok {
		l.markImportExport(modulePath, name)
		return addressTarget{expr: ir.Expression{Kind: ir.ExprAddressOf, ModulePath: modulePath, Export: name}}, true
	}
	switch expr.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(expr.Name)
		if local, _, ok := l.lookupLocal(name, scope); ok {
			return addressTarget{expr: ir.Expression{Kind: ir.ExprAddressOf, Local: local}}, true
		}
		if upvalue, ok := l.resolveUpvalue(name, scope); ok {
			return addressTarget{expr: ir.Expression{Kind: ir.ExprAddressOf, Upvalue: upvalue}}, true
		}
		if global, ok := l.globals[name]; ok {
			return addressTarget{expr: ir.Expression{Kind: ir.ExprAddressOf, Global: global}}, true
		}
		l.add(addressDiagnostic(assignment, "target"), "unknown address target identifier", expr.Span)
		return addressTarget{}, false
	case ast.ExprDeref:
		if expr.Operand == nil {
			l.add(addressDiagnostic(assignment, "deref.missing"), "dereference target requires an operand", expr.Span)
			return addressTarget{}, false
		}
		pointer, ok := l.lowerExpression(*expr.Operand, scope)
		if !ok {
			return addressTarget{}, false
		}
		pointerType := firstNonEmpty(l.typeRefString(pointer.Type), l.expressionType(*expr.Operand, scope))
		if !l.isPointerType(pointerType) {
			l.add(addressDiagnostic(assignment, "deref.type"), "dereference target requires a pointer", expr.Span)
			return addressTarget{}, false
		}
		local := l.newSyntheticLocal(scope, "addr.pointer", pointerType)
		return addressTarget{
			expr: ir.Expression{Kind: ir.ExprAddressOf, Local: local, Path: []ir.AddressSegment{{Kind: "indirect"}}},
			lets: []addressLet{{local: local, value: pointer}},
		}, true
	case ast.ExprSelector:
		if expr.Operand == nil || strings.TrimSpace(expr.Field) == "" {
			l.add(addressDiagnostic(assignment, "member.missing"), "member target requires object and field", expr.Span)
			return addressTarget{}, false
		}
		if l.rejectAmbiguousSelector(expr, scope, "hirgen.selector.ambiguous") {
			return addressTarget{}, false
		}
		fields := []string{expr.Field}
		if selection, ok := l.semanticSelection(expr); ok && selection.Kind == check.SelectionField && len(selection.Index) > 1 {
			path, _, pathOK := l.semanticSelectionPath(selection)
			if !pathOK {
				l.add(addressDiagnostic(assignment, "member.path"), "field selector is missing its semantic path", expr.Span)
				return addressTarget{}, false
			}
			fields = path
		}
		segments := make([]ir.AddressSegment, 0, len(fields))
		for _, field := range fields {
			segments = append(segments, ir.AddressSegment{Kind: "field", Field: field})
		}
		operandType := l.expressionType(*expr.Operand, scope)
		if l.isPointerType(operandType) {
			pointer, ok := l.lowerExpression(*expr.Operand, scope)
			if !ok {
				return addressTarget{}, false
			}
			local := l.newSyntheticLocal(scope, "addr.pointer", operandType)
			return addressTarget{
				expr: ir.Expression{Kind: ir.ExprAddressOf, Local: local, Path: segments},
				lets: []addressLet{{local: local, value: pointer}},
			}, true
		}
		target, ok := l.lowerAddressTargetFor(*expr.Operand, scope, assignment)
		if !ok {
			return addressTarget{}, false
		}
		target.expr.Path = append(target.expr.Path, segments...)
		return target, true
	case ast.ExprIndex:
		if expr.Operand == nil || expr.Index == nil {
			l.add(addressDiagnostic(assignment, "index.missing"), "index target requires object and index", expr.Span)
			return addressTarget{}, false
		}
		objectType := l.expressionType(*expr.Operand, scope)
		if l.isStringType(objectType) {
			l.add(addressDiagnostic(assignment, "index.string"), "cannot take or assign through a string index", expr.Span)
			return addressTarget{}, false
		}
		if _, _, ok := l.mapKeyValueTypes(objectType); ok {
			l.add(addressDiagnostic(assignment, "index.map"), "cannot take address of map index", expr.Span)
			return addressTarget{}, false
		}
		index, ok := l.lowerIndexExpression(*expr.Index, "Int", scope)
		if !ok {
			return addressTarget{}, false
		}
		indexLocal := l.newSyntheticLocal(scope, "addr.index", "Int")
		if l.isSliceType(objectType) || l.pointerArrayTypeOK(objectType) {
			object, ok := l.lowerExpression(*expr.Operand, scope)
			if !ok {
				return addressTarget{}, false
			}
			objectLocal := l.newSyntheticLocal(scope, "addr.object", objectType)
			return addressTarget{
				expr: ir.Expression{Kind: ir.ExprAddressOf, Local: objectLocal, Path: []ir.AddressSegment{{Kind: "index", Local: indexLocal}}},
				lets: []addressLet{
					{local: objectLocal, value: object},
					{local: indexLocal, value: index},
				},
			}, true
		}
		if !l.isArrayType(objectType) {
			l.add(addressDiagnostic(assignment, "index.type"), "index target requires an array, pointer to array, or slice", expr.Span)
			return addressTarget{}, false
		}
		target, ok := l.lowerAddressTargetFor(*expr.Operand, scope, assignment)
		if !ok {
			return addressTarget{}, false
		}
		target.lets = append(target.lets, addressLet{local: indexLocal, value: index})
		target.expr.Path = append(target.expr.Path, ir.AddressSegment{Kind: "index", Local: indexLocal})
		return target, true
	default:
		message := "address-of requires an addressable identifier, pointer dereference, selector, array index, or slice index"
		if assignment {
			message = "assignment target is not addressable"
		}
		l.add(addressDiagnostic(assignment, "target"), message, expr.Span)
		return addressTarget{}, false
	}
}

func addressDiagnostic(assignment bool, suffix string) string {
	if assignment {
		return "hirgen.assign." + suffix
	}
	return "hirgen.addr." + suffix
}
