package specialize

import "github.com/d7z-team/mini-go/compiler/ast"

func (s *genericSpecializer) rewriteGenericFunctionValue(expr *ast.Expression, target ast.TypeExpr, substitutions map[string]ast.TypeExpr) {
	if expr == nil {
		return
	}
	target = s.underlyingTypeExpr(cloneGenericType(target), map[string]bool{})
	substituteGenericType(&target, substitutions)
	if target.Kind != ast.TypeFunc {
		return
	}
	name := genericCalleeName(*expr)
	var explicit []ast.TypeExpr
	if expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList {
		name, explicit = genericInstantiation(*expr, substitutions)
	}
	methodName, receiver, methodArgs := s.prepareGenericMethod(*expr, substitutions)
	if methodName != "" {
		name, explicit = methodName, methodArgs
	}
	generic, ok := s.functions[name]
	if !ok || len(explicit) > len(generic.typeParams) {
		return
	}
	bindings := make(map[string]ast.TypeExpr, len(generic.typeParams))
	for i := range explicit {
		bindings[generic.typeParams[i].Name] = explicit[i]
	}
	pattern := ast.TypeExpr{Kind: ast.TypeFunc, Params: generic.decl.Func.Params, Results: generic.decl.Func.Results, Span: expr.Span}
	if receiver != nil {
		pattern.Params = pattern.Params[1:]
	}
	if !s.inferGenericType(pattern, target, generic.typeParams, bindings) || !s.inferConstraintBindings(generic, bindings) {
		return
	}
	args := make([]ast.TypeExpr, 0, len(generic.typeParams))
	for _, param := range generic.typeParams {
		arg, found := bindings[param.Name]
		if !found {
			return
		}
		args = append(args, arg)
	}
	generated := s.instantiateFunction(name, args, expr.Span)
	if generated == "" {
		return
	}

	if methodName != "" {
		s.bindMethodValue(expr, generated, receiver)
	} else {
		*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprIdent, Name: generated, Type: &target, Span: expr.Span}
	}
}

func (s *genericSpecializer) rewriteSemanticCallArgumentContexts(expr *ast.Expression, substitutions map[string]ast.TypeExpr) {
	if expr == nil || expr.Callee == nil || s.activeAlias != "" {
		return
	}
	if _, generic := s.functions[genericCalleeName(*expr.Callee)]; generic {
		return
	}
	call, ok := s.info.Calls[expr.NodeID]
	if !ok || len(call.Signature.Params) == 0 {
		return
	}
	params := make([]ast.Field, 0, len(call.Signature.Params))
	for _, param := range call.Signature.Params {
		params = append(params, ast.Field{Type: s.sourceTypeExpr(param.Type, expr.Span)})
	}
	if call.Signature.Variadic && len(params) != 0 {
		params[len(params)-1].Variadic = true
	}
	s.rewriteCallArgumentContexts(expr.Args, params, substitutions, expr.Ellipsis)
}

func (s *genericSpecializer) rewriteCallArgumentContexts(args []ast.Expression, params []ast.Field, substitutions map[string]ast.TypeExpr, ellipsis bool) {
	for i := range args {
		paramIndex := i
		if len(params) != 0 && params[len(params)-1].Variadic && paramIndex >= len(params)-1 {
			paramIndex = len(params) - 1
		}
		if paramIndex >= len(params) {
			return
		}
		target := cloneGenericType(params[paramIndex].Type)
		substituteGenericType(&target, substitutions)
		if params[paramIndex].Variadic && !(ellipsis && i == len(args)-1) && target.Kind == ast.TypeSlice && target.Elem != nil {
			target = *target.Elem
		}
		s.rewriteGenericFunctionValue(&args[i], target, substitutions)
	}
}

func (s *genericSpecializer) rewriteCompositeFunctionValues(expr *ast.Expression, substitutions map[string]ast.TypeExpr) {
	if expr.Type == nil {
		return
	}
	target := s.underlyingTypeExpr(cloneGenericType(*expr.Type), map[string]bool{})
	substituteGenericType(&target, substitutions)
	switch target.Kind {
	case ast.TypeArray, ast.TypeSlice, ast.TypeMap:
		if target.Elem == nil {
			return
		}
		for i := range expr.Items {
			s.rewriteGenericFunctionValue(&expr.Items[i].Value, *target.Elem, substitutions)
		}
	case ast.TypeStruct:
		fields := make(map[string]ast.TypeExpr, len(target.Fields))
		for _, field := range target.Fields {
			fields[field.Name] = field.Type
		}
		for i := range expr.Items {
			if expr.Items[i].Key != nil {
				fieldType := fields[expr.Items[i].Key.Name]
				if composite, ok := s.info.Composites[expr.NodeID]; ok && i < len(composite.Initializers) && composite.Initializers[i].Type.Valid() {
					fieldType = s.sourceTypeExpr(composite.Initializers[i].Type, expr.Items[i].Value.Span)
				}
				s.rewriteGenericFunctionValue(&expr.Items[i].Value, fieldType, substitutions)
			} else if i < len(target.Fields) {
				s.rewriteGenericFunctionValue(&expr.Items[i].Value, target.Fields[i].Type, substitutions)
			}
		}
	}
}

func (s *genericSpecializer) setGeneratedCallResult(expr *ast.Expression, generated string) {
	decl, ok := s.generatedFunc[generated]
	if !ok || len(decl.Results) != 1 {
		return
	}
	target := cloneGenericType(decl.Results[0].Type)
	s.canonicalizeTypeImports(&target)
	expr.Type = &target
}
