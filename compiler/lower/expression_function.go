package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
)

func (l *lowerer) lowerExpressionsInLocalDeclTypes(decl ast.ValueDecl, scope *funcScope) ([]ir.Expression, bool) {
	out := make([]ir.Expression, 0, len(decl.Values))
	for i, value := range decl.Values {
		if l.resolveSourceType(decl.Type) == "" && isNilLiteral(value) {
			l.add("hirgen.var.nil", "variable initializer cannot use untyped nil without an explicit type", value.Span)
			return nil, false
		}
		targetType := ""
		if i < len(decl.Names) {
			targetType = l.localDeclType(decl, i, scope)
		}
		lowered, ok := l.lowerExpressionInType(value, targetType, scope)
		if !ok {
			return nil, false
		}
		out = append(out, lowered)
	}
	return out, true
}

func (l *lowerer) lowerExpressionsInAssignmentTypes(values, targets []ast.Expression, scope *funcScope) ([]ir.Expression, bool) {
	out := make([]ir.Expression, 0, len(values))
	for i, value := range values {
		targetType := ""
		if i < len(targets) {
			targetType = l.assignmentTargetType(targets[i], scope)
		}
		lowered, ok := l.lowerExpressionInType(value, targetType, scope)
		if !ok {
			return nil, false
		}
		out = append(out, lowered)
	}
	return out, true
}

func (l *lowerer) assignmentTargetType(target ast.Expression, scope *funcScope) string {
	if typ, category, ok := l.semanticValueFact(target); ok && category.Assignable() && typ != "" {
		return typ
	}
	switch target.Kind {
	case ast.ExprIdent:
		name := strings.TrimSpace(target.Name)
		if name == "" || name == "_" {
			return ""
		}
		if _, typ, ok := l.lookupLocal(name, scope); ok {
			return typ
		}
		if _, typ, ok := l.lookupUpvalue(name, scope); ok {
			return typ
		}
		if _, ok := l.resolveUpvalue(name, scope); ok {
			_, typ, _ := l.lookupUpvalue(name, scope)
			return typ
		}
		return l.typeRefString(l.globalTypes[name])
	case ast.ExprDeref:
		if target.Operand == nil {
			return ""
		}
		elem, _ := l.pointerElementType(l.expressionType(*target.Operand, scope))
		return elem
	case ast.ExprIndex:
		if target.Operand == nil {
			return ""
		}
		return l.indexElementType(l.expressionType(*target.Operand, scope))
	case ast.ExprSelector:
		if target.Operand == nil {
			return ""
		}
		return l.memberType(l.expressionType(*target.Operand, scope), target.Field)
	default:
		return ""
	}
}

func (l *lowerer) lowerFuncLiteral(expr ast.Expression, outer *funcScope) (ir.Expression, bool) {
	l.nextAnonFunc++
	fn := ir.Function{
		ID:            fmt.Sprintf("fn.literal.%d", l.nextAnonFunc),
		Name:          fmt.Sprintf("literal.%d", l.nextAnonFunc),
		RevisionLocal: true,
		Declaration:   hirLocationPtr(expr.Span),
		Signature:     l.hirSignature(l.signatureOf(*expr.Func), funcDeclVariadic(*expr.Func)),
	}
	scope := newFuncScope(&fn, outer)
	scope.resultTypes = l.typeRefsFromStrings(l.resultTypes(expr.Func.Results))
	scope.expectedReturns = len(expr.Func.Results)
	for _, param := range expr.Func.Params {
		if !l.addFunctionParamLocal(&fn, &scope, param, len(fn.Locals)) {
			return ir.Expression{}, false
		}
	}
	l.collectNamedResults(expr.Func.Results, &fn, &scope)
	body, ok := l.lowerBlockInScope(expr.Func.Body, &scope)
	if !ok {
		return ir.Expression{}, false
	}
	fn.Body = append(fn.Body, body...)
	captures := make([]ir.CaptureTarget, 0, len(fn.Upvalues))
	for _, upvalue := range fn.Upvalues {
		capture, ok := scope.captures[upvalue.Name]
		if !ok {
			l.add("hirgen.func_literal.capture.missing", "function literal capture was not recorded", expr.Span)
			return ir.Expression{}, false
		}
		captures = append(captures, capture)
	}
	l.extraFunctions = append(l.extraFunctions, fn)
	return ir.Expression{Kind: ir.ExprFunction, Function: fn.ID, Captures: captures}, true
}

func (l *lowerer) resolveUpvalue(name string, scope *funcScope) (string, bool) {
	if scope == nil || scope.outer == nil || scope.function == nil {
		return "", false
	}
	owner := l.functionRootScope(scope)
	if upvalue, ok := owner.upvalues[name]; ok {
		return upvalue, true
	}
	outer := scope.outer
	for outer != nil && outer.function == scope.function {
		outer = outer.outer
	}
	capture, typ, ok := l.resolveCaptureTarget(name, outer)
	if !ok {
		return "", false
	}
	if typ == "" {
		typ = "Any"
	}
	variadic := l.functionTypeVariadic(typ) || l.lookupBindingVariadic(name, outer)
	upvalue := "up." + name
	owner.upvalues[name] = upvalue
	owner.upvalueTypes[name] = l.hirType(typ)
	owner.upvalueVariadics[name] = variadic
	owner.captures[name] = capture
	scope.function.Upvalues = append(scope.function.Upvalues, ir.Upvalue{
		ID:   upvalue,
		Name: name,
		Type: l.hirType(typ),
	})
	return upvalue, true
}

func (l *lowerer) functionRootScope(scope *funcScope) *funcScope {
	if scope == nil {
		return nil
	}
	root := scope
	for root.outer != nil && root.outer.function == scope.function {
		root = root.outer
	}
	return root
}

func (l *lowerer) resolveCaptureTarget(name string, scope *funcScope) (ir.CaptureTarget, string, bool) {
	if scope == nil {
		return ir.CaptureTarget{}, "", false
	}
	if local, typ, ok := l.lookupLocal(name, scope); ok {
		return ir.CaptureTarget{Kind: "local", Local: local}, typ, true
	}
	if upvalue, typ, ok := l.lookupUpvalue(name, scope); ok {
		return ir.CaptureTarget{Kind: "upvalue", Upvalue: upvalue}, typ, true
	}
	if upvalue, ok := l.resolveUpvalue(name, scope); ok {
		_, typ, _ := l.lookupUpvalue(name, scope)
		return ir.CaptureTarget{Kind: "upvalue", Upvalue: upvalue}, typ, true
	}
	return ir.CaptureTarget{}, "", false
}
