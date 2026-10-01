package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
)

func (l *lowerer) lowerIncDecAssign(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	operator, ok := incDecCompoundOperator(stmt.Op)
	if !ok {
		l.add("hirgen.assign.incdec.op", "unsupported increment/decrement assignment operator", stmt.Span)
		return nil, false
	}
	return l.lowerCompoundAssign(ast.Statement{
		Kind: stmt.Kind,
		Span: stmt.Span,
		Op:   operator,
		Left: stmt.Left,
		Right: []ast.Expression{{
			Kind:    ast.ExprLiteral,
			Literal: "1",
			Type:    &ast.TypeExpr{Kind: ast.TypeName, Name: "Int"},
			Span:    stmt.Span,
		}},
	}, scope)
}

func (l *lowerer) lowerCompoundAssign(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	if len(stmt.Left) != 1 || len(stmt.Right) != 1 {
		l.add("hirgen.assign.compound.shape", "compound assignment requires exactly one target and one value", stmt.Span)
		return nil, false
	}
	operator, ok := compoundOperator(stmt.Op)
	if !ok {
		l.add("hirgen.assign.compound.op", "unsupported compound assignment operator", stmt.Span)
		return nil, false
	}
	target := stmt.Left[0]
	plan, ok := l.planLValue(target, scope)
	if !ok {
		return nil, false
	}
	current, ok := plan.load(l)
	if !ok {
		l.add("hirgen.assign.compound.target", "compound assignment target is not writable", target.Span)
		return nil, false
	}
	if operatorSelection, overloaded := l.semanticOperator(stmt.NodeID); overloaded {
		value, lowered := l.lowerCompoundOperator(operatorSelection.Selection, plan, current, stmt.Right[0], scope)
		if !lowered {
			return nil, false
		}
		out := append([]ir.Statement(nil), plan.prelude...)
		out = append(out, plan.store(value))
		return out, true
	}
	rightType := plan.typ
	if isShiftOperator(operator) {
		rightType = l.expressionType(stmt.Right[0], scope)
	}
	right, ok := l.lowerExpressionInType(stmt.Right[0], rightType, scope)
	if !ok {
		return nil, false
	}
	if hirResultCount(right) != 1 {
		l.add("hirgen.assign.compound.result", "compound assignment value must produce exactly one result", stmt.Span)
		return nil, false
	}
	value := ir.Expression{
		Kind:     ir.ExprBinary,
		Operator: operator,
		Type:     l.hirType(plan.typ),
		Left:     &current,
		Right:    &right,
	}
	out := append([]ir.Statement(nil), plan.prelude...)
	out = append(out, plan.store(value))
	return out, true
}

func (l *lowerer) lowerCompoundOperator(selection check.Selection, plan lvaluePlan, current ir.Expression, right ast.Expression, scope *funcScope) (ir.Expression, bool) {
	path, receiverType, ok := l.semanticSelectionPath(selection)
	if !ok {
		return ir.Expression{}, false
	}
	if selection.Interface {
		method, found := l.semanticInterfaceMethodFromSelection(selection)
		if !found {
			return ir.Expression{}, false
		}
		receiver := projectSelectorPath(current, path)
		args, lowered := l.lowerOperatorArguments(&right, l.signatureParamTypes(method.Signature), scope, nil)
		if !lowered {
			return ir.Expression{}, false
		}
		return ir.Expression{
			Kind: ir.ExprCallInterface, Type: method.InterfaceType, Field: method.Method,
			Operand: &receiver, Args: args, ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
		}, true
	}
	method, found := l.methodInfoFromSelection(selection)
	if !found {
		return ir.Expression{}, false
	}
	receiver := projectSelectorPath(current, path)
	if selection.Indirect {
		receiver, ok = plan.addressForMethod(path)
		if !ok {
			return ir.Expression{}, false
		}
	} else {
		receiver = l.autoDerefMethodReceiver(receiver, receiverType, method)
	}
	args, ok := l.lowerOperatorArguments(&right, l.signatureParamTypes(method.Signature), scope, []ir.Expression{receiver})
	if !ok {
		return ir.Expression{}, false
	}
	return ir.Expression{
		Kind: ir.ExprCallDirect, ModulePath: method.ModulePath, Function: method.FunctionID,
		Args: args, ResultCount: len(method.Signature.Results), ResultTypes: method.Signature.Results,
	}, true
}

func (plan lvaluePlan) addressForMethod(path []string) (ir.Expression, bool) {
	segments := make([]ir.AddressSegment, len(path))
	for i, field := range path {
		segments[i] = ir.AddressSegment{Kind: "field", Field: field}
	}
	switch plan.kind {
	case lvalueSlot:
		return ir.Expression{
			Kind: ir.ExprAddressOf, Local: plan.slot.Local, Upvalue: plan.slot.Upvalue,
			Global: plan.slot.Global, Path: segments,
		}, true
	case lvalueAddress:
		if plan.address.Kind != ir.ExprLocal || plan.address.Local == "" {
			return ir.Expression{}, false
		}
		return ir.Expression{Kind: ir.ExprAddressOf, Local: plan.address.Local, Path: segments}, true
	default:
		return ir.Expression{}, false
	}
}

func (l *lowerer) hasMapIndexRoot(target ast.Expression, scope *funcScope) bool {
	switch target.Kind {
	case ast.ExprIndex:
		if target.Operand == nil {
			return false
		}
		if _, _, ok := l.mapKeyValueTypes(l.expressionType(*target.Operand, scope)); ok {
			return !l.isPointerType(l.expressionType(target, scope))
		}
		return l.hasMapIndexRoot(*target.Operand, scope)
	case ast.ExprSelector, ast.ExprDeref:
		return target.Operand != nil && l.hasMapIndexRoot(*target.Operand, scope)
	default:
		return false
	}
}

func (l *lowerer) lowerLocalDecl(decl ast.Decl, scope *funcScope) ([]ir.Statement, bool) {
	switch decl.Kind {
	case ast.DeclVar:
		return l.lowerLocalVarDecl(decl, scope)
	case ast.DeclConst:
		if !l.lowerLocalConstDecl(decl, scope) {
			return nil, false
		}
		return nil, true
	case ast.DeclType:
		return nil, true
	default:
		l.add("hirgen.stmt.decl.unsupported", "unsupported local declaration in AST emit", decl.Span)
		return nil, false
	}
}

func (l *lowerer) lowerLocalVarDecl(decl ast.Decl, scope *funcScope) ([]ir.Statement, bool) {
	valueDecl := *decl.Var
	if !l.validateLocalVarNames(valueDecl, decl.Span, scope) {
		return nil, false
	}
	if !l.validateVarDeclaration(valueDecl, decl.Span, scope) {
		return nil, false
	}
	if len(valueDecl.Values) == 0 {
		if !l.declareLocalVars(valueDecl, decl.Span, scope) {
			return nil, false
		}
		out := make([]ir.Statement, 0, len(valueDecl.Names))
		for _, name := range valueDecl.Names {
			if strings.TrimSpace(name) == "_" {
				continue
			}
			local, typ, ok := l.lookupLocal(name, scope)
			if !ok {
				return nil, false
			}
			out = append(out, ir.Statement{Kind: ir.StmtStoreLocal, Local: local, Rebind: true, Expr: ir.Expression{Kind: ir.ExprZero, Type: l.hirType(typ)}})
		}
		return out, true
	}
	if len(valueDecl.Names) > 1 && len(valueDecl.Values) == 1 {
		value, ok := l.lowerMultiResultValue(valueDecl.Values[0], len(valueDecl.Names), scope, "hirgen.var.results")
		if !ok {
			return nil, false
		}
		targetTypes := make([]string, len(valueDecl.Names))
		for i := range targetTypes {
			targetTypes[i] = l.localDeclType(valueDecl, i, scope)
		}
		if !l.validateMultiResultTargets(value, targetTypes, "hirgen.assign.type", valueDecl.Values[0]) {
			return nil, false
		}
		if !multiResultNeedsNormalization(value.types, targetTypes) {
			if !l.declareLocalVars(valueDecl, decl.Span, scope) {
				return nil, false
			}
			targets, ok := l.localStoreTargets(valueDecl.Names, scope, decl.Span)
			if !ok {
				return nil, false
			}
			markLocalTargetsForRebind(targets, nil, nil)
			return []ir.Statement{{Kind: ir.StmtStoreResults, Expr: value.expr, Targets: targets}}, true
		}
		binding, ok := l.bindMultiResult(value, targetTypes, scope, "hirgen.assign.type", valueDecl.Values[0])
		if !ok || !l.declareLocalVars(valueDecl, decl.Span, scope) {
			return nil, false
		}
		targets, ok := l.localStoreTargets(valueDecl.Names, scope, decl.Span)
		if !ok {
			return nil, false
		}
		markLocalTargetsForRebind(targets, nil, nil)
		return []ir.Statement{
			binding.capture(),
			{Kind: ir.StmtStoreValues, Values: binding.values, Targets: targets},
		}, true
	}
	values, ok := l.lowerExpressionsInLocalDeclTypes(valueDecl, scope)
	if !ok {
		return nil, false
	}
	for _, value := range values {
		if hirResultCount(value) != 1 {
			l.add("hirgen.var.shape", "multi-valued expression must be the only var initializer", decl.Span)
			return nil, false
		}
	}
	if len(values) != len(valueDecl.Names) {
		l.add("hirgen.var.shape", "var declaration initializer result count must match names", decl.Span)
		return nil, false
	}
	if !l.declareLocalVars(valueDecl, decl.Span, scope) {
		return nil, false
	}
	targets, ok := l.localStoreTargets(valueDecl.Names, scope, decl.Span)
	if !ok {
		return nil, false
	}
	markLocalTargetsForRebind(targets, nil, nil)
	if len(values) == 1 {
		return []ir.Statement{{Kind: ir.StmtStoreResults, Expr: values[0], Targets: targets}}, true
	}
	return []ir.Statement{{Kind: ir.StmtStoreValues, Values: values, Targets: targets}}, true
}

func (l *lowerer) validateLocalVarNames(decl ast.ValueDecl, span source.Span, scope *funcScope) bool {
	seen := map[string]struct{}{}
	for _, name := range decl.Names {
		name = strings.TrimSpace(name)
		if name == "" || name == "_" {
			continue
		}
		if scope == nil || scope.function == nil {
			l.add("hirgen.var.scope", "local declaration requires function scope", span)
			return false
		}
		if _, exists := seen[name]; exists {
			l.add("hirgen.local.duplicate", "duplicate local name", span)
			return false
		}
		seen[name] = struct{}{}
		if _, exists := scope.locals[name]; exists {
			l.add("hirgen.local.duplicate", "duplicate local name", span)
			return false
		}
		if _, exists := scope.constants[name]; exists {
			l.add("hirgen.local.duplicate", "duplicate local name", span)
			return false
		}
	}
	return true
}

func (l *lowerer) lowerShortVarDecl(stmt ast.Statement, scope *funcScope) ([]ir.Statement, bool) {
	if scope == nil || scope.function == nil {
		l.add("hirgen.short.scope", "short variable declaration requires function scope", stmt.Span)
		return nil, false
	}
	if len(stmt.Left) == 0 || len(stmt.Right) == 0 {
		l.add("hirgen.short.shape", "short variable declaration requires left and right expressions", stmt.Span)
		return nil, false
	}
	names := make([]string, len(stmt.Left))
	newTargets := map[string]bool{}
	seen := map[string]struct{}{}
	newName := false
	for i, target := range stmt.Left {
		if target.Kind != ast.ExprIdent {
			l.add("hirgen.short.target", "short variable declaration requires identifier targets", target.Span)
			return nil, false
		}
		name := strings.TrimSpace(target.Name)
		names[i] = name
		if name == "" {
			l.add("hirgen.short.target", "short variable declaration requires identifier targets", target.Span)
			return nil, false
		}
		if name == "_" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			l.add("hirgen.short.duplicate", "short variable declaration has duplicate target name", target.Span)
			return nil, false
		}
		seen[name] = struct{}{}
		if _, exists := scope.constants[name]; exists {
			l.add("hirgen.short.constant", "short variable declaration cannot redeclare a constant", target.Span)
			return nil, false
		}
		if _, exists := l.currentLocal(name, scope); !exists {
			newName = true
			newTargets[name] = true
		}
	}
	if !newName {
		l.add("hirgen.short.no_new", "short variable declaration requires at least one new variable", stmt.Span)
		return nil, false
	}
	if len(stmt.Left) > 1 && len(stmt.Right) == 1 {
		value, ok := l.lowerMultiResultValue(stmt.Right[0], len(stmt.Left), scope, "hirgen.short.results")
		if !ok {
			return nil, false
		}
		targetTypes := make([]string, len(names))
		for i, name := range names {
			if typ, exists := l.currentLocal(name, scope); exists {
				targetTypes[i] = typ
			} else {
				targetTypes[i] = value.types[i]
			}
		}
		if !l.validateMultiResultTargets(value, targetTypes, "hirgen.assign.type", stmt.Right[0]) {
			return nil, false
		}
		var binding multiResultBinding
		if multiResultNeedsNormalization(value.types, targetTypes) {
			binding, ok = l.bindMultiResult(value, targetTypes, scope, "hirgen.assign.type", stmt.Right[0])
			if !ok {
				return nil, false
			}
		}
		if !l.declareShortVarTargets(names, stmt.Left, stmt.Right, scope) {
			return nil, false
		}
		targets, ok := l.localStoreTargets(names, scope, stmt.Span)
		if !ok {
			return nil, false
		}
		markLocalTargetsForRebind(targets, names, newTargets)
		if len(binding.values) == 0 {
			return []ir.Statement{{Kind: ir.StmtStoreResults, Expr: value.expr, Targets: targets}}, true
		}
		return []ir.Statement{
			binding.capture(),
			{Kind: ir.StmtStoreValues, Values: binding.values, Targets: targets},
		}, true
	}
	if len(stmt.Right) != len(stmt.Left) {
		l.add("hirgen.short.shape", "short variable declaration initializer count must match names", stmt.Span)
		return nil, false
	}
	values, ok := l.lowerShortVarValues(stmt, scope)
	if !ok {
		return nil, false
	}
	if !l.declareShortVarTargets(names, stmt.Left, stmt.Right, scope) {
		return nil, false
	}
	targets, ok := l.localStoreTargets(names, scope, stmt.Span)
	if !ok {
		return nil, false
	}
	markLocalTargetsForRebind(targets, names, newTargets)
	if len(values) == 1 {
		return []ir.Statement{{Kind: ir.StmtStoreResults, Expr: values[0], Targets: targets}}, true
	}
	return []ir.Statement{{Kind: ir.StmtStoreValues, Values: values, Targets: targets}}, true
}

func (l *lowerer) lowerShortVarValues(stmt ast.Statement, scope *funcScope) ([]ir.Expression, bool) {
	out := make([]ir.Expression, 0, len(stmt.Right))
	for i, value := range stmt.Right {
		targetType := ""
		if i < len(stmt.Left) && stmt.Left[i].Kind == ast.ExprIdent {
			if typ, ok := l.currentLocal(stmt.Left[i].Name, scope); ok {
				targetType = typ
			} else {
				if isNilLiteral(value) {
					l.add("hirgen.short.nil", "short variable declaration cannot infer a type from untyped nil", value.Span)
					return nil, false
				}
				if l.untypedConstExpression(value, scope) {
					targetType = l.defaultedExpressionType(value, scope)
				}
			}
		}
		lowered, ok := l.lowerExpressionInType(value, targetType, scope)
		if !ok {
			return nil, false
		}
		out = append(out, lowered)
	}
	return out, true
}

func (l *lowerer) declareShortVarTargets(names []string, targets, values []ast.Expression, scope *funcScope) bool {
	for i, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == "_" {
			continue
		}
		if _, exists := l.currentLocal(name, scope); exists {
			continue
		}
		typ := l.shortVarTargetType(values, i, scope)
		variadic := l.functionTypeVariadic(typ)
		id := l.newLocalID(scope.function, name)
		var declaration *ir.Location
		if i < len(targets) {
			declaration = hirLocationPtr(targets[i].Span)
		}
		scope.function.Locals = append(scope.function.Locals, ir.Local{
			ID: id, Name: name, Type: l.hirType(typ), Scope: scope.debugScope, Declaration: declaration,
		})
		scope.locals[name] = id
		scope.localTypes[name] = l.hirType(typ)
		scope.localVariadics[name] = variadic
		l.recordFunctionValueType(scope, name, typ, variadic)
	}
	return true
}

func (l *lowerer) shortVarTargetType(values []ast.Expression, index int, scope *funcScope) string {
	if len(values) == 1 {
		if typ := l.multiResultValueType(values[0], index, scope); typ != "" {
			return typ
		}
		if index == 0 {
			if typ := l.defaultedExpressionType(values[0], scope); typ != "" {
				return typ
			}
		}
		return "Any"
	}
	if index >= 0 && index < len(values) {
		if typ := l.defaultedExpressionType(values[index], scope); typ != "" {
			return typ
		}
	}
	return "Any"
}
