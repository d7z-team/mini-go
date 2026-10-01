package specialize

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (s *genericSpecializer) rewriteDecl(decl *ast.Decl, substitutions map[string]ast.TypeExpr) {
	switch decl.Kind {
	case ast.DeclConst:
		s.rewriteValueDecl(decl.Const, decl.NodeID, substitutions)
	case ast.DeclVar:
		s.rewriteValueDecl(decl.Var, decl.NodeID, substitutions)
	case ast.DeclType:
		s.rewriteType(&decl.Type.Type, substitutions)
	case ast.DeclFunc:
		s.rewriteFunc(decl.Func, substitutions)
	}
}

func (s *genericSpecializer) rewriteValueDecl(decl *ast.ValueDecl, node ast.NodeID, substitutions map[string]ast.TypeExpr) {
	s.rewriteType(&decl.Type, substitutions)
	for i := range decl.Values {
		if decl.Type.Kind != ast.TypeInvalid {
			s.rewriteGenericFunctionValue(&decl.Values[i], decl.Type, substitutions)
		}
		s.rewriteExpr(&decl.Values[i], substitutions)
	}
	if decl.Type.Kind != ast.TypeInvalid {
		return
	}
	valueTypes := s.assignmentTypes(decl.Values, len(decl.Names), substitutions)
	for i, name := range decl.Names {
		if i >= len(valueTypes) || valueTypes[i].Kind == ast.TypeInvalid {
			continue
		}
		for _, id := range s.info.Defs[node] {
			if s.info.Objects[id].Name == name {
				s.valueTypes[id] = valueTypes[i]
			}
		}
	}
}

func (s *genericSpecializer) rewriteFunc(decl *ast.FuncDecl, substitutions map[string]ast.TypeExpr) {
	if decl.Receiver != nil {
		s.rewriteType(&decl.Receiver.Type, substitutions)
	}
	for i := range decl.Params {
		s.rewriteType(&decl.Params[i].Type, substitutions)
	}
	for i := range decl.Results {
		s.rewriteType(&decl.Results[i].Type, substitutions)
	}
	previousResults := s.resultTypes
	s.resultTypes = make([]ast.TypeExpr, 0, len(decl.Results))
	for i := range decl.Results {
		s.resultTypes = append(s.resultTypes, decl.Results[i].Type)
	}
	s.rewriteBlock(&decl.Body, substitutions)
	s.resultTypes = previousResults
}

func (s *genericSpecializer) rewriteType(typ *ast.TypeExpr, substitutions map[string]ast.TypeExpr) {
	if typ == nil || typ.Kind == ast.TypeInvalid {
		return
	}
	if typ.Kind == ast.TypeName {
		if replacement, ok := substitutions[typ.Name]; ok {
			span, nodeID := typ.Span, typ.NodeID
			*typ = cloneGenericType(replacement)
			typ.Span, typ.NodeID = span, nodeID
			return
		}
		if s.activeAlias != "" && !strings.Contains(typ.Name, ".") {
			if _, ok := s.activeNamedTypes[typ.Name]; ok {
				typ.Name = s.activeAlias + "." + typ.Name
			}
		}

		return
	}
	s.rewriteType(typ.Base, substitutions)
	for i := range typ.TypeArgs {
		s.rewriteType(&typ.TypeArgs[i], substitutions)
	}
	if typ.Kind == ast.TypeInstance && typ.Base != nil && typ.Base.Kind == ast.TypeName {
		name := typ.Base.Name
		if generated := s.instantiateType(name, typ.TypeArgs, typ.Span); generated != "" {
			*typ = ast.TypeExpr{NodeID: typ.NodeID, Kind: ast.TypeName, Name: generated, Span: typ.Span}
			return
		}
	}
	s.rewriteType(typ.Elem, substitutions)
	s.rewriteType(typ.Key, substitutions)
	if typ.Len != nil {
		s.rewriteExpr(typ.Len, substitutions)
	}
	for i := range typ.Params {
		s.rewriteType(&typ.Params[i].Type, substitutions)
	}
	for i := range typ.Results {
		s.rewriteType(&typ.Results[i].Type, substitutions)
	}
	for i := range typ.Fields {
		if typ.Fields[i].Name == "" && typ.Fields[i].EmbeddedName == "" {
			typ.Fields[i].EmbeddedName = genericReceiverName(typ.Fields[i].Type)
		}
		s.rewriteType(&typ.Fields[i].Type, substitutions)
	}
	for i := range typ.Methods {
		s.rewriteFunc(&typ.Methods[i], substitutions)
	}
	for i := range typ.Embeds {
		s.rewriteType(&typ.Embeds[i], substitutions)
	}
	for i := range typ.Terms {
		s.rewriteType(&typ.Terms[i].Type, substitutions)
	}
}

func (s *genericSpecializer) rewriteBlock(block *ast.BlockStmt, substitutions map[string]ast.TypeExpr) {
	if block == nil {
		return
	}
	for i := range block.Stmts {
		s.rewriteStmt(&block.Stmts[i], substitutions)
	}
}

func (s *genericSpecializer) rewriteStmt(stmt *ast.Statement, substitutions map[string]ast.TypeExpr) {
	if stmt == nil {
		return
	}
	for i := range stmt.Decls {
		s.rewriteDecl(&stmt.Decls[i], substitutions)
	}
	s.rewriteExpr(stmt.Expr, substitutions)
	for i := range stmt.Left {
		s.rewriteExpr(&stmt.Left[i], substitutions)
	}
	if len(stmt.Left) == len(stmt.Right) {
		for i := range stmt.Right {
			// Newly declared variables obtain their concrete type from the
			// rewritten initializer, not its provisional generic source type.
			if len(s.info.Defs[stmt.Left[i].NodeID]) != 0 {
				continue
			}
			if target, ok := s.expressionType(stmt.Left[i], substitutions); ok {
				if stmt.Kind == ast.StmtSend {
					target = s.underlyingTypeExpr(target, map[string]bool{})
					if target.Kind == ast.TypeChan && target.Elem != nil {
						target = *target.Elem
					}
				}
				s.rewriteGenericFunctionValue(&stmt.Right[i], target, substitutions)
			}
		}
	}
	for i := range stmt.Right {
		s.rewriteExpr(&stmt.Right[i], substitutions)
	}
	if stmt.Kind == ast.StmtAssign && stmt.Op == ":=" {
		valueTypes := s.assignmentTypes(stmt.Right, len(stmt.Left), substitutions)
		for i := range stmt.Left {
			if stmt.Left[i].Kind != ast.ExprIdent || i >= len(valueTypes) || valueTypes[i].Kind == ast.TypeInvalid {
				continue
			}
			for _, objectID := range s.info.Defs[stmt.Left[i].NodeID] {
				s.valueTypes[objectID] = valueTypes[i]
			}
		}
	}
	s.rewriteBlock(&stmt.Body, substitutions)
	s.rewriteStmt(stmt.Init, substitutions)
	s.rewriteExpr(stmt.Cond, substitutions)
	s.rewriteStmt(stmt.Post, substitutions)
	s.rewriteStmt(stmt.Else, substitutions)
	s.rewriteExpr(stmt.Key, substitutions)
	s.rewriteExpr(stmt.Value, substitutions)
	s.rewriteExpr(stmt.Range, substitutions)
	for i := range stmt.Cases {
		clause := &stmt.Cases[i]
		for j := range clause.Values {
			s.rewriteExpr(&clause.Values[j], substitutions)
		}
		for j := range clause.Types {
			s.rewriteType(&clause.Types[j], substitutions)
		}
		s.rewriteStmt(clause.Comm, substitutions)
		s.rewriteBlock(&clause.Body, substitutions)
	}
	for i := range stmt.Results {
		if i < len(s.resultTypes) {
			s.rewriteGenericFunctionValue(&stmt.Results[i], s.resultTypes[i], substitutions)
		}
		s.rewriteExpr(&stmt.Results[i], substitutions)
	}
}

func (s *genericSpecializer) rewriteExpr(expr *ast.Expression, substitutions map[string]ast.TypeExpr) {
	if expr == nil || expr.Kind == ast.ExprInvalid {
		return
	}
	if expr.FunctionID != "" {
		s.rewriteType(expr.Type, substitutions)
		return
	}
	if s.rewriteGenericMethodCall(expr, substitutions) {
		return
	}
	if expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList {
		if name, receiver, args := s.prepareGenericMethod(*expr, substitutions); name != "" && len(args) == len(s.functions[name].typeParams) {
			if generated := s.instantiateFunction(name, args, expr.Span); generated != "" {
				s.bindMethodValue(expr, generated, receiver)
			}
			return
		}
	}
	if expr.Kind == ast.ExprCall && s.activeAlias != "" && expr.Callee != nil && expr.Callee.Kind == ast.ExprIdent && len(expr.Args) == 1 {
		if replacement, ok := substitutions[expr.Callee.Name]; ok {
			s.rewriteExpr(&expr.Args[0], substitutions)
			operand := expr.Args[0]
			target := cloneGenericType(replacement)
			*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprConvert, Type: &target, Operand: &operand, Span: expr.Span}
			return
		}
	}
	if expr.Kind == ast.ExprIdent {
		object := s.info.Objects[s.info.Uses[expr.NodeID]]
		if replacement, ok := substitutions[expr.Name]; ok && (s.activeAlias != "" || object.Kind == check.ObjectTypeParam) {
			expr.Name = "type"
			target := cloneGenericType(replacement)
			expr.Type = &target
			expr.Type.Span = expr.Span
			return
		}
	}
	s.rewriteType(expr.Type, substitutions)
	if call, ok := s.info.Calls[expr.NodeID]; ok && call.Kind == check.CallConversion && len(expr.Args) == 1 {
		s.rewriteGenericFunctionValue(&expr.Args[0], s.sourceTypeExpr(call.Target, expr.Span), substitutions)
	}
	if call, ok := s.info.Calls[expr.NodeID]; ok && call.Kind == check.CallConversion && call.Target.Kind == types.TypeParameter && len(expr.Args) == 1 {
		s.rewriteExpr(&expr.Args[0], substitutions)
		target := s.sourceTypeExpr(call.Target, expr.Span)
		s.rewriteType(&target, substitutions)
		operand := expr.Args[0]
		*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprConvert, Span: expr.Span, Type: &target, Operand: &operand}
		return
	}
	if expr.Kind == ast.ExprCall {
		s.rewriteSemanticCallArgumentContexts(expr, substitutions)
	}
	if expr.Kind == ast.ExprComposite {
		s.rewriteCompositeFunctionValues(expr, substitutions)
	}
	if expr.Kind == ast.ExprCall && expr.Callee != nil && (expr.Callee.Kind == ast.ExprIndex || expr.Callee.Kind == ast.ExprIndexList) {
		for i := range expr.Args {
			s.rewriteExpr(&expr.Args[i], substitutions)
		}
		name, explicit := genericInstantiation(*expr.Callee, substitutions)
		generic, ok := s.functions[name]
		if ok {
			typeArgs, inferred := s.inferTypeArgs(generic, expr.Args, substitutions, explicit, expr.Ellipsis)
			if !inferred {
				s.addDiagnostic("compiler.generic.inference", "cannot infer type arguments for "+name, expr.Span)
				return
			}
			s.rewriteCallArgumentContexts(expr.Args, generic.decl.Func.Params, bindTypeArgs(generic.typeParams, typeArgs), expr.Ellipsis)
			if generated := s.instantiateFunction(name, typeArgs, expr.Span); generated != "" {
				expr.Callee = &ast.Expression{NodeID: expr.Callee.NodeID, Kind: ast.ExprIdent, Name: generated, Span: expr.Callee.Span}
				s.setGeneratedCallResult(expr, generated)
			}
			return
		}
	}
	s.rewriteExpr(expr.Left, substitutions)
	s.rewriteExpr(expr.Right, substitutions)
	s.rewriteExpr(expr.Operand, substitutions)
	s.rewriteExpr(expr.Callee, substitutions)
	for i := range expr.Args {
		s.rewriteExpr(&expr.Args[i], substitutions)
	}
	s.rewriteExpr(expr.Index, substitutions)
	s.rewriteExpr(expr.Start, substitutions)
	s.rewriteExpr(expr.End, substitutions)
	s.rewriteExpr(expr.Max, substitutions)
	for i := range expr.Items {
		s.rewriteExpr(expr.Items[i].Key, substitutions)
		s.rewriteExpr(&expr.Items[i].Value, substitutions)
	}
	if expr.Kind == ast.ExprFunc {
		s.rewriteFunc(expr.Func, substitutions)
	}
	if (expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList) && expr.Operand != nil {
		name := genericCalleeName(*expr.Operand)
		var typeArgs []ast.TypeExpr
		if expr.Kind == ast.ExprIndex && expr.Index != nil {
			if typ, ok := genericExprType(*expr.Index, substitutions); ok {
				typeArgs = []ast.TypeExpr{typ}
			}
		} else {
			for _, arg := range expr.Args {
				typ, ok := genericExprType(arg, substitutions)
				if !ok {
					typeArgs = nil
					break
				}
				typeArgs = append(typeArgs, typ)
			}
		}
		if len(typeArgs) != 0 {
			if generated := s.instantiateFunction(name, typeArgs, expr.Span); generated != "" {
				*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprIdent, Name: generated, Span: expr.Span}
				return
			}
			if generated := s.instantiateType(name, typeArgs, expr.Span); generated != "" {
				*expr = ast.Expression{NodeID: expr.NodeID, Kind: ast.ExprIdent, Name: generated, Type: &ast.TypeExpr{Kind: ast.TypeName, Name: generated, Span: expr.Span}, Span: expr.Span}
				return
			}
		}
	}
	if expr.Kind == ast.ExprCall && expr.Callee != nil {
		name := genericCalleeName(*expr.Callee)
		generic, ok := s.functions[name]
		if !ok {
			return
		}
		inferred, ok := s.inferTypeArgs(generic, expr.Args, substitutions, nil, expr.Ellipsis)
		if !ok {
			s.addDiagnostic("compiler.generic.inference", "cannot infer type arguments for "+name, expr.Span)
			return
		}
		s.rewriteCallArgumentContexts(expr.Args, generic.decl.Func.Params, bindTypeArgs(generic.typeParams, inferred), expr.Ellipsis)
		if generated := s.instantiateFunction(name, inferred, expr.Span); generated != "" {
			expr.Callee = &ast.Expression{NodeID: expr.Callee.NodeID, Kind: ast.ExprIdent, Name: generated, Span: expr.Callee.Span}
			s.setGeneratedCallResult(expr, generated)
		}
	}
}

func genericInstantiation(expr ast.Expression, substitutions map[string]ast.TypeExpr) (string, []ast.TypeExpr) {
	if (expr.Kind != ast.ExprIndex && expr.Kind != ast.ExprIndexList) || expr.Operand == nil {
		return "", nil
	}
	name := genericCalleeName(*expr.Operand)
	if expr.Kind == ast.ExprIndex && expr.Index != nil {
		typ, ok := genericExprType(*expr.Index, substitutions)
		if !ok {
			return name, nil
		}
		return name, []ast.TypeExpr{typ}
	}
	typeArgs := make([]ast.TypeExpr, 0, len(expr.Args))
	for _, arg := range expr.Args {
		typ, ok := genericExprType(arg, substitutions)
		if !ok {
			return name, nil
		}
		typeArgs = append(typeArgs, typ)
	}
	return name, typeArgs
}
