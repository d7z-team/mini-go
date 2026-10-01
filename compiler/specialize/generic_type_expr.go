package specialize

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func genericExprType(expr ast.Expression, substitutions map[string]ast.TypeExpr) (ast.TypeExpr, bool) {
	if expr.Kind == ast.ExprIdent {
		if expr.Name == "type" && expr.Type != nil && expr.Type.Kind != ast.TypeInvalid {
			typ := cloneGenericType(*expr.Type)
			substituteGenericType(&typ, substitutions)
			return typ, true
		}
		if replacement, ok := substitutions[expr.Name]; ok {
			return replacement, true
		}
		return ast.TypeExpr{Kind: ast.TypeName, Name: canonicalGenericTypeName(expr.Name), Span: expr.Span}, true
	}
	if expr.Kind == ast.ExprSelector && expr.Operand != nil && expr.Operand.Kind == ast.ExprIdent {
		name := expr.Operand.Name + "." + expr.Field
		return ast.TypeExpr{Kind: ast.TypeName, Name: name, Span: expr.Span}, true
	}
	if (expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList) && expr.Operand != nil {
		base, ok := genericExprType(*expr.Operand, substitutions)
		if !ok {
			return ast.TypeExpr{}, false
		}
		var expressions []ast.Expression
		if expr.Kind == ast.ExprIndex && expr.Index != nil {
			expressions = []ast.Expression{*expr.Index}
		} else {
			expressions = expr.Args
		}
		args := make([]ast.TypeExpr, 0, len(expressions))
		for _, expression := range expressions {
			arg, ok := genericExprType(expression, substitutions)
			if !ok {
				return ast.TypeExpr{}, false
			}
			args = append(args, arg)
		}
		return ast.TypeExpr{Kind: ast.TypeInstance, Base: &base, TypeArgs: args, Span: expr.Span}, len(args) != 0
	}
	return ast.TypeExpr{}, false
}

func substituteGenericType(typ *ast.TypeExpr, substitutions map[string]ast.TypeExpr) {
	if typ == nil {
		return
	}
	if typ.Kind == ast.TypeName {
		if replacement, ok := substitutions[typ.Name]; ok {
			span, nodeID := typ.Span, typ.NodeID
			*typ = cloneGenericType(replacement)
			typ.Span, typ.NodeID = span, nodeID
		}
		return
	}
	substituteGenericType(typ.Base, substitutions)
	for i := range typ.TypeArgs {
		substituteGenericType(&typ.TypeArgs[i], substitutions)
	}
	substituteGenericType(typ.Elem, substitutions)
	substituteGenericType(typ.Key, substitutions)
	for i := range typ.Params {
		substituteGenericType(&typ.Params[i].Type, substitutions)
	}
	for i := range typ.Results {
		substituteGenericType(&typ.Results[i].Type, substitutions)
	}
	for i := range typ.Fields {
		substituteGenericType(&typ.Fields[i].Type, substitutions)
	}
	for i := range typ.Methods {
		for j := range typ.Methods[i].Params {
			substituteGenericType(&typ.Methods[i].Params[j].Type, substitutions)
		}
		for j := range typ.Methods[i].Results {
			substituteGenericType(&typ.Methods[i].Results[j].Type, substitutions)
		}
	}
	for i := range typ.Embeds {
		substituteGenericType(&typ.Embeds[i], substitutions)
	}
	for i := range typ.Terms {
		substituteGenericType(&typ.Terms[i].Type, substitutions)
	}
}

func bindTypeArgs(params []ast.TypeParam, args []ast.TypeExpr) map[string]ast.TypeExpr {
	out := make(map[string]ast.TypeExpr, len(params))
	for i := range params {
		if params[i].Name != "_" && i < len(args) {
			out[params[i].Name] = cloneGenericType(args[i])
		}
	}
	return out
}

func cloneTypeBindings(bindings map[string]ast.TypeExpr) map[string]ast.TypeExpr {
	out := make(map[string]ast.TypeExpr, len(bindings))
	for name, typ := range bindings {
		out[name] = cloneGenericType(typ)
	}
	return out
}

func (s *genericSpecializer) canonicalizeTypeImports(typ *ast.TypeExpr) {
	if typ == nil || typ.Kind == ast.TypeInvalid {
		return
	}
	if typ.Kind == ast.TypeName {
		if dot := strings.IndexByte(typ.Name, '.'); dot > 0 {
			if modulePath := strings.TrimSpace(s.importPaths[typ.Name[:dot]]); modulePath != "" {
				typ.Name = modulePath + typ.Name[dot:]
			}
		}
		return
	}
	s.canonicalizeTypeImports(typ.Base)
	for i := range typ.TypeArgs {
		s.canonicalizeTypeImports(&typ.TypeArgs[i])
	}
	s.canonicalizeTypeImports(typ.Elem)
	s.canonicalizeTypeImports(typ.Key)
	for _, fields := range [][]ast.Field{typ.Params, typ.Results, typ.Fields} {
		for i := range fields {
			s.canonicalizeTypeImports(&fields[i].Type)
		}
	}
	for i := range typ.Methods {
		for j := range typ.Methods[i].Params {
			s.canonicalizeTypeImports(&typ.Methods[i].Params[j].Type)
		}
		for j := range typ.Methods[i].Results {
			s.canonicalizeTypeImports(&typ.Methods[i].Results[j].Type)
		}
	}
	for i := range typ.Embeds {
		s.canonicalizeTypeImports(&typ.Embeds[i])
	}
	for i := range typ.Terms {
		s.canonicalizeTypeImports(&typ.Terms[i].Type)
	}
}

func cloneGenericType(typ ast.TypeExpr) ast.TypeExpr {
	out := typ
	if typ.Base != nil {
		base := cloneGenericType(*typ.Base)
		out.Base = &base
	}
	if typ.Elem != nil {
		elem := cloneGenericType(*typ.Elem)
		out.Elem = &elem
	}
	if typ.Key != nil {
		key := cloneGenericType(*typ.Key)
		out.Key = &key
	}
	out.TypeArgs = cloneGenericTypes(typ.TypeArgs)
	out.Params = cloneGenericFields(typ.Params)
	out.Results = cloneGenericFields(typ.Results)
	out.Fields = cloneGenericFields(typ.Fields)
	out.Embeds = cloneGenericTypes(typ.Embeds)
	out.Terms = append([]ast.TypeTerm(nil), typ.Terms...)
	for i := range out.Terms {
		out.Terms[i].Type = cloneGenericType(out.Terms[i].Type)
	}
	out.Methods = append([]ast.FuncDecl(nil), typ.Methods...)
	for i := range out.Methods {
		out.Methods[i].Params = cloneGenericFields(out.Methods[i].Params)
		out.Methods[i].Results = cloneGenericFields(out.Methods[i].Results)
	}
	return out
}

func cloneGenericTypes(types []ast.TypeExpr) []ast.TypeExpr {
	out := make([]ast.TypeExpr, len(types))
	for i := range types {
		out[i] = cloneGenericType(types[i])
	}
	return out
}

func cloneGenericFields(fields []ast.Field) []ast.Field {
	out := append([]ast.Field(nil), fields...)
	for i := range out {
		out[i].Type = cloneGenericType(out[i].Type)
	}
	return out
}

func genericArrayLength(typ ast.TypeExpr) string {
	if typ.Len == nil {
		return ""
	}
	if typ.Len.Kind == ast.ExprLiteral {
		return strings.TrimSpace(typ.Len.Literal)
	}
	return strconv.FormatUint(uint64(typ.Len.NodeID), 10)
}

func (s *genericSpecializer) underlyingTypeExpr(typ ast.TypeExpr, seen map[string]bool) ast.TypeExpr {
	name := genericTypeText(typ)
	if seen[name] {
		return typ
	}
	if definition, ok := s.typeDefs[name]; ok {
		seen[name] = true
		return s.underlyingTypeExpr(cloneGenericType(definition), seen)
	}
	if typ.Kind == ast.TypeInstance && typ.Base != nil {
		generic, ok := s.types[genericTypeText(*typ.Base)]
		if ok && len(generic.typeParams) == len(typ.TypeArgs) {
			definition := cloneGenericType(generic.decl.Type.Type)
			substituteGenericType(&definition, bindTypeArgs(generic.typeParams, typ.TypeArgs))
			return s.underlyingTypeExpr(definition, seen)
		}
	}
	return typ
}

func substituteReceiverTypeParams(receiver *ast.Field, params []ast.TypeParam, args []ast.TypeExpr, substitutions map[string]ast.TypeExpr) {
	if receiver == nil {
		return
	}
	typ := &receiver.Type
	if typ.Kind == ast.TypePointer && typ.Elem != nil {
		typ = typ.Elem
	}
	if typ.Kind != ast.TypeInstance {
		return
	}
	for i := range typ.TypeArgs {
		if i < len(params) {
			substitutions[typ.TypeArgs[i].Name] = args[i]
		}
	}
}

func setSpecializedReceiver(receiver *ast.Field, name string) {
	if receiver == nil {
		return
	}
	span, nodeID := receiver.Type.Span, receiver.Type.NodeID
	concrete := ast.TypeExpr{NodeID: nodeID, Kind: ast.TypeName, Name: name, Span: span}
	if receiver.Type.Kind == ast.TypePointer {
		receiver.Type = ast.TypeExpr{NodeID: nodeID, Kind: ast.TypePointer, Elem: &concrete, Span: span}
		return
	}
	receiver.Type = concrete
}
