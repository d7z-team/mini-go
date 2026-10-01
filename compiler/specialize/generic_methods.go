package specialize

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

// prepareGenericMethod turns a selected declaration into a generic free
// function with an explicit receiver. Receiver type arguments are bound here;
// the ordinary function specializer handles method type arguments.
func (s *genericSpecializer) prepareGenericMethod(expr ast.Expression, substitutions map[string]ast.TypeExpr) (string, *ast.Expression, []ast.TypeExpr) {
	var explicit []ast.TypeExpr
	if expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList {
		_, explicit = genericInstantiation(expr, substitutions)
		if expr.Operand == nil {
			return "", nil, nil
		}
		expr = *expr.Operand
	}
	if expr.Kind != ast.ExprSelector || expr.Operand == nil {
		return "", nil, nil
	}
	selection := s.info.Selections[expr.NodeID]
	if selection.Kind == check.SelectionPackageMember {
		return "", nil, nil
	}
	receiver := *expr.Operand
	s.rewriteExpr(&receiver, substitutions)
	receiverType, ok := s.expressionType(receiver, substitutions)
	if !ok {
		return "", nil, nil
	}
	methodExpression := selection.Kind == check.SelectionMethodExpression || s.info.Exprs[expr.Operand.NodeID].Mode == check.ExprType
	expressionReceiverType := receiverType
	if methodExpression {
		receiver = ast.Expression{Kind: ast.ExprIdent, Name: "_method_outer_receiver", Type: &receiverType, Span: expr.Span}
	}
	for _, index := range selection.Index {
		underlying := s.underlyingTypeExpr(receiverType, map[string]bool{})
		if underlying.Kind == ast.TypePointer && underlying.Elem != nil {
			underlying = s.underlyingTypeExpr(*underlying.Elem, map[string]bool{})
		}
		if index >= len(underlying.Fields) {
			return "", nil, nil
		}
		field := underlying.Fields[index]
		if field.Name == "" {
			field.Name = field.EmbeddedName
			if field.Name == "" {
				field.Name = genericReceiverName(field.Type)
			}
		}
		previous := receiver
		receiver = ast.Expression{Kind: ast.ExprSelector, Operand: &previous, Field: field.Name, Type: &field.Type, Span: expr.Span}
		receiverType = field.Type
	}
	base := receiverType
	pointer := base.Kind == ast.TypePointer && base.Elem != nil
	if pointer {
		base = *base.Elem
	}
	owner := base.Name
	var receiverArgs []ast.TypeExpr
	if instance, found := s.typeInstances[owner]; found {
		owner, receiverArgs = instance.base, instance.args
	}
	if base.Kind == ast.TypeInstance && base.Base != nil {
		owner, receiverArgs = base.Base.Name, base.TypeArgs
	}
	owner = strings.TrimPrefix(owner, s.info.ModulePath+".")
	for alias, path := range s.importPaths {
		if strings.HasPrefix(owner, path+".") {
			owner = alias + strings.TrimPrefix(owner, path)
			break
		}
	}
	for _, template := range s.methods[owner] {
		if template.decl.Func.Name != expr.Field || len(template.decl.Func.TypeParams) == 0 {
			continue
		}
		method := cloneGenericDecl(template.decl)
		methodPointer := method.Func.Receiver.Type.Kind == ast.TypePointer
		if methodExpression && methodPointer && !pointer && expressionReceiverType.Kind != ast.TypePointer {
			return "", nil, nil
		}
		bindings := make(map[string]ast.TypeExpr)
		if generic, found := s.types[owner]; found {
			substituteReceiverTypeParams(method.Func.Receiver, generic.typeParams, receiverArgs, bindings)
		}
		// Bind the receiver parameters throughout the body before materializing
		// the method. Method parameters remain available to ordinary inference.
		ast.WalkTypes(&ast.Program{Files: []ast.File{{Decls: []ast.Decl{method}}}}, func(typ *ast.TypeExpr) { substituteGenericType(typ, bindings) })
		recv := *method.Func.Receiver
		recv.Type = base
		if methodPointer {
			recv.Type = ast.TypeExpr{Kind: ast.TypePointer, Elem: &base, Span: expr.Span}
		}
		if recv.Name == "" || recv.Name == "_" {
			recv.Name = "_method_receiver"
		}
		method.Func.Receiver = nil
		method.Func.Params = append([]ast.Field{recv}, method.Func.Params...)
		if methodPointer && !pointer {
			previous := receiver
			receiver = ast.Expression{Kind: ast.ExprAddr, Operand: &previous, Type: &recv.Type, Span: expr.Span}
		} else if !methodPointer && pointer {
			previous := receiver
			receiver = ast.Expression{Kind: ast.ExprDeref, Operand: &previous, Type: &recv.Type, Span: expr.Span}
		}
		key := genericTypeText(base) + "." + expr.Field
		if methodExpression && len(selection.Index) > 0 {
			method.Func.Params[0] = ast.Field{Name: "_method_outer_receiver", Type: expressionReceiverType, Span: expr.Span}
			bind := ast.Statement{Kind: ast.StmtAssign, Op: ":=", Left: []ast.Expression{{Kind: ast.ExprIdent, Name: recv.Name, Span: expr.Span}}, Right: []ast.Expression{receiver}, Span: expr.Span}
			method.Func.Body.Stmts = append([]ast.Statement{bind}, method.Func.Body.Stmts...)
			key = genericTypeText(expressionReceiverType) + ".promoted." + expr.Field
		}
		prepared := template
		prepared.decl, prepared.typeParams = method, method.Func.TypeParams
		prepared.receiverBindings = bindings
		s.functions[key] = prepared
		if methodExpression {
			// The caller supplies the receiver as the first argument.
			return key, nil, explicit
		}
		return key, &receiver, explicit
	}
	return "", nil, nil
}

func (s *genericSpecializer) rewriteGenericMethodCall(expr *ast.Expression, substitutions map[string]ast.TypeExpr) bool {
	if expr.Kind != ast.ExprCall || expr.Callee == nil {
		return false
	}
	name, receiver, explicit := s.prepareGenericMethod(*expr.Callee, substitutions)
	if name == "" {
		return false
	}
	for i := range expr.Args {
		s.rewriteExpr(&expr.Args[i], substitutions)
	}
	args := expr.Args
	if receiver != nil {
		args = append([]ast.Expression{*receiver}, args...)
	}
	generic := s.functions[name]
	typeArgs, ok := s.inferTypeArgs(generic, args, substitutions, explicit, expr.Ellipsis)
	if !ok {
		s.addDiagnostic("compiler.generic.inference", "cannot infer type arguments for "+name, expr.Span)
		return true
	}
	s.rewriteCallArgumentContexts(args, generic.decl.Func.Params, bindTypeArgs(generic.typeParams, typeArgs), expr.Ellipsis)
	if generated := s.instantiateFunction(name, typeArgs, expr.Span); generated != "" {
		expr.Callee = &ast.Expression{Kind: ast.ExprIdent, Name: generated, Span: expr.Callee.Span}
		expr.Args = args
		s.setGeneratedCallResult(expr, generated)
	}
	return true
}

// bindMethodValue uses one receiver argument to a capture factory. The inner
// closure therefore retains a value copy or pointer at method-value creation.
func (s *genericSpecializer) bindMethodValue(expr *ast.Expression, generated string, receiver *ast.Expression) {
	if receiver == nil {
		*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprIdent, Name: generated, Span: expr.Span}
		return
	}
	decl := s.generatedFunc[generated]
	params := append([]ast.Field(nil), decl.Params[1:]...)
	args := []ast.Expression{{Kind: ast.ExprIdent, Name: "_method_receiver", Span: expr.Span}}
	for i := range params {
		params[i].Name = "_method_arg_" + strconv.Itoa(i)
		args = append(args, ast.Expression{Kind: ast.ExprIdent, Name: params[i].Name, Span: expr.Span})
	}
	call := ast.Expression{Kind: ast.ExprCall, Callee: &ast.Expression{Kind: ast.ExprIdent, Name: generated, Span: expr.Span}, Args: args, Span: expr.Span}
	call.Ellipsis = len(params) > 0 && params[len(params)-1].Variadic
	statement := ast.Statement{Kind: ast.StmtReturn, Results: []ast.Expression{call}, Span: expr.Span}
	if len(decl.Results) == 0 {
		statement = ast.Statement{Kind: ast.StmtExpr, Expr: &call, Span: expr.Span}
	}
	functionType := cloneGenericType(ast.TypeExpr{Kind: ast.TypeFunc, Params: params, Results: decl.Results, Span: expr.Span})
	s.canonicalizeTypeImports(&functionType)
	receiverType := cloneGenericType(decl.Params[0].Type)
	s.canonicalizeTypeImports(&receiverType)
	closure := ast.Expression{Kind: ast.ExprFunc, Span: expr.Span, Func: &ast.FuncDecl{Params: functionType.Params, Results: functionType.Results, Body: ast.BlockStmt{Stmts: []ast.Statement{statement}}}}
	factory := ast.Expression{Kind: ast.ExprFunc, Span: expr.Span, Func: &ast.FuncDecl{
		Params:  []ast.Field{{Name: "_method_receiver", Type: receiverType}},
		Results: []ast.Field{{Type: functionType}},
		Body:    ast.BlockStmt{Stmts: []ast.Statement{{Kind: ast.StmtReturn, Results: []ast.Expression{closure}, Span: expr.Span}}},
	}}
	*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprCall, Callee: &factory, Args: []ast.Expression{*receiver}, Type: &functionType, Span: expr.Span}
}
