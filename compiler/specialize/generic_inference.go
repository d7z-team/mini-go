package specialize

import (
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (s *genericSpecializer) inferTypeArgs(generic genericDecl, args []ast.Expression, substitutions map[string]ast.TypeExpr, explicit []ast.TypeExpr, ellipsis bool) ([]ast.TypeExpr, bool) {
	if len(explicit) > len(generic.typeParams) {
		return nil, false
	}
	bindings := make(map[string]ast.TypeExpr, len(generic.typeParams))
	for i := range explicit {
		arg := cloneGenericType(explicit[i])
		substituteGenericType(&arg, substitutions)
		bindings[generic.typeParams[i].Name] = arg
	}
	params := generic.decl.Func.Params
	for i := range args {
		if args[i].Kind == ast.ExprLiteral && strings.TrimSpace(args[i].Literal) == "nil" {
			continue
		}
		paramIndex := i
		if len(params) != 0 && params[len(params)-1].Variadic && paramIndex >= len(params)-1 {
			paramIndex = len(params) - 1
		}
		if paramIndex >= len(params) {
			break
		}
		if s.genericFunctionValue(args[i], substitutions) {
			continue
		}
		actual, ok := s.expressionType(args[i], substitutions)
		if !ok {
			continue
		}
		pattern := cloneGenericType(params[paramIndex].Type)
		s.qualifyImportedType(&pattern, generic.importAlias)
		if params[paramIndex].Variadic && !(ellipsis && i == len(args)-1) && pattern.Kind == ast.TypeSlice && pattern.Elem != nil {
			pattern = *pattern.Elem
		}
		if !s.inferGenericType(pattern, actual, generic.typeParams, bindings) {
			return nil, false
		}
	}
	if !s.inferConstraintBindings(generic, bindings) {
		return nil, false
	}
	out := make([]ast.TypeExpr, 0, len(generic.typeParams))
	for _, param := range generic.typeParams {
		arg, ok := bindings[param.Name]
		if !ok {
			return nil, false
		}
		out = append(out, arg)
	}
	return out, true
}

func (s *genericSpecializer) genericFunctionValue(expr ast.Expression, substitutions map[string]ast.TypeExpr) bool {
	name := genericCalleeName(expr)
	if expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprIndexList {
		name, _ = genericInstantiation(expr, substitutions)
	}
	_, ok := s.functions[name]
	return ok
}

func (s *genericSpecializer) inferConstraintBindings(generic genericDecl, bindings map[string]ast.TypeExpr) bool {
	for pass := 0; pass < len(generic.typeParams); pass++ {
		before := len(bindings)
		for i := range generic.typeParams {
			actual, ok := bindings[generic.typeParams[i].Name]
			if !ok {
				continue
			}
			constraint := cloneGenericType(generic.typeParams[i].Constraint)
			s.qualifyImportedType(&constraint, generic.importAlias)
			if !s.inferConstraintType(actual, constraint, generic, bindings, map[string]bool{}) {
				return false
			}
		}
		if len(bindings) == before {
			break
		}
	}
	return true
}

func (s *genericSpecializer) qualifyImportedType(typ *ast.TypeExpr, alias string) {
	if typ == nil || alias == "" {
		return
	}
	if typ.Kind == ast.TypeName {
		name := typ.Name
		if !strings.Contains(name, ".") {
			qualified := alias + "." + name
			if _, ok := s.typeDefs[qualified]; ok {
				typ.Name = qualified
			}
		}
	}
	if typ.Kind == ast.TypeInstance && typ.Base != nil && typ.Base.Kind == ast.TypeName {
		name := typ.Base.Name
		if !strings.Contains(name, ".") {
			qualified := alias + "." + name
			if _, ok := s.types[qualified]; ok {
				typ.Base.Name = qualified
			}
		}
	}
	s.qualifyImportedType(typ.Base, alias)
	s.qualifyImportedType(typ.Elem, alias)
	s.qualifyImportedType(typ.Key, alias)
	for i := range typ.TypeArgs {
		s.qualifyImportedType(&typ.TypeArgs[i], alias)
	}
	for _, fields := range [][]ast.Field{typ.Params, typ.Results, typ.Fields} {
		for i := range fields {
			s.qualifyImportedType(&fields[i].Type, alias)
		}
	}
	for i := range typ.Embeds {
		s.qualifyImportedType(&typ.Embeds[i], alias)
	}
	for i := range typ.Terms {
		s.qualifyImportedType(&typ.Terms[i].Type, alias)
	}
}

func (s *genericSpecializer) inferConstraintType(actual, constraint ast.TypeExpr, generic genericDecl, bindings map[string]ast.TypeExpr, seen map[string]bool) bool {
	name := constraint.Name
	if constraint.Kind == ast.TypeName {
		if name == "any" || name == "Any" || name == "comparable" {
			return true
		}
		if seen[name] {
			return true
		}
		resolved, ok := generic.namedTypes[name]
		if !ok {
			resolved, ok = s.typeDefs[name]
		}
		if !ok {
			return true
		}
		seen[name] = true
		ok = s.inferConstraintType(actual, resolved, generic, bindings, seen)
		delete(seen, name)
		return ok
	}
	if constraint.Kind == ast.TypeInstance && constraint.Base != nil {
		constraintName := genericTypeText(*constraint.Base)
		decl, ok := s.types[constraintName]
		if !ok || len(decl.typeParams) != len(constraint.TypeArgs) {
			return true
		}
		args := make([]ast.TypeExpr, len(constraint.TypeArgs))
		for i := range constraint.TypeArgs {
			args[i] = cloneGenericType(constraint.TypeArgs[i])
		}
		for i := range args {
			substituteGenericType(&args[i], bindings)
		}
		resolved := cloneGenericType(decl.decl.Type.Type)
		substituteGenericType(&resolved, bindTypeArgs(decl.typeParams, args))
		return s.inferConstraintType(actual, resolved, decl, bindings, seen)
	}
	if constraint.Kind != ast.TypeInterface {
		s.inferGenericType(constraint, actual, generic.typeParams, bindings)
		return true
	}
	if len(constraint.Terms) != 0 {
		matched := false
		for _, term := range constraint.Terms {
			candidate := actual
			if term.Approx {
				candidate = s.underlyingTypeExpr(actual, map[string]bool{})
			}
			trial := cloneTypeBindings(bindings)
			if s.inferGenericType(term.Type, candidate, generic.typeParams, trial) {
				for key, value := range trial {
					bindings[key] = value
				}
				matched = true
				break
			}
		}
		if !matched {
			return true
		}
	}
	for _, embedded := range constraint.Embeds {
		s.inferConstraintType(actual, embedded, generic, bindings, seen)
	}
	methods := s.methodSets[genericTypeText(actual)]
	for _, required := range constraint.Methods {
		method, ok := methods[required.Name]
		if !ok || len(method.decl.Params) != len(required.Params) || len(method.decl.Results) != len(required.Results) {
			continue
		}
		trial := cloneTypeBindings(bindings)
		matched := true
		for i := range required.Params {
			if !s.inferGenericType(required.Params[i].Type, method.decl.Params[i].Type, generic.typeParams, trial) {
				matched = false
				break
			}
		}
		for i := 0; matched && i < len(required.Results); i++ {
			if !s.inferGenericType(required.Results[i].Type, method.decl.Results[i].Type, generic.typeParams, trial) {
				matched = false
				break
			}
		}
		if matched {
			for key, value := range trial {
				bindings[key] = value
			}
		}
	}
	return true
}

func (s *genericSpecializer) inferGenericType(pattern, actual ast.TypeExpr, params []ast.TypeParam, bindings map[string]ast.TypeExpr) bool {
	if pattern.Kind == ast.TypeName {
		name := pattern.Name
		for _, param := range params {
			if param.Name != name {
				continue
			}
			if existing, ok := bindings[name]; ok {
				return genericTypeText(existing) == genericTypeText(actual)
			}
			bindings[name] = actual
			return true
		}
	}
	if pattern.Kind == ast.TypeInstance && pattern.Base != nil && actual.Kind == ast.TypeName {
		instance, ok := s.typeInstances[genericTypeText(actual)]
		if ok && instance.base == genericTypeText(*pattern.Base) && len(pattern.TypeArgs) == len(instance.args) {
			for i := range pattern.TypeArgs {
				if !s.inferGenericType(pattern.TypeArgs[i], instance.args[i], params, bindings) {
					return false
				}
			}
			return true
		}
	}
	if pattern.Kind != actual.Kind {
		if definition, ok := s.typeDefs[genericTypeText(actual)]; ok {
			actual = definition
		}
	}
	if pattern.Kind != actual.Kind {
		return false
	}
	if pattern.Kind == ast.TypeName {
		return canonicalGenericTypeName(pattern.Name) == canonicalGenericTypeName(actual.Name)
	}
	if pattern.Kind == ast.TypeInstance {
		if pattern.Base == nil || actual.Base == nil || genericTypeText(*pattern.Base) != genericTypeText(*actual.Base) || len(pattern.TypeArgs) != len(actual.TypeArgs) {
			return false
		}
	}
	if pattern.Kind == ast.TypeArray && genericArrayLength(pattern) != genericArrayLength(actual) {
		return false
	}
	if pattern.Elem != nil || actual.Elem != nil {
		if pattern.Elem == nil || actual.Elem == nil || !s.inferGenericType(*pattern.Elem, *actual.Elem, params, bindings) {
			return false
		}
	}
	if pattern.Key != nil || actual.Key != nil {
		if pattern.Key == nil || actual.Key == nil || !s.inferGenericType(*pattern.Key, *actual.Key, params, bindings) {
			return false
		}
	}
	if len(pattern.TypeArgs) != len(actual.TypeArgs) || len(pattern.Params) != len(actual.Params) || len(pattern.Results) != len(actual.Results) {
		return false
	}
	for i := range pattern.TypeArgs {
		if !s.inferGenericType(pattern.TypeArgs[i], actual.TypeArgs[i], params, bindings) {
			return false
		}
	}
	for i := range pattern.Params {
		if pattern.Params[i].Variadic != actual.Params[i].Variadic || !s.inferGenericType(pattern.Params[i].Type, actual.Params[i].Type, params, bindings) {
			return false
		}
	}
	for i := range pattern.Results {
		if !s.inferGenericType(pattern.Results[i].Type, actual.Results[i].Type, params, bindings) {
			return false
		}
	}
	if len(pattern.Fields) != len(actual.Fields) {
		return false
	}
	for i := range pattern.Fields {
		if pattern.Fields[i].Name != actual.Fields[i].Name || pattern.Fields[i].Tag != actual.Fields[i].Tag || !s.inferGenericType(pattern.Fields[i].Type, actual.Fields[i].Type, params, bindings) {
			return false
		}
	}
	return true
}

func (s *genericSpecializer) expressionType(expr ast.Expression, substitutions map[string]ast.TypeExpr) (ast.TypeExpr, bool) {
	if expr.Kind == ast.ExprIdent {
		if typ, ok := s.valueTypes[s.info.Uses[expr.NodeID]]; ok {
			return cloneGenericType(typ), true
		}
	}
	if expr.Operand != nil && (expr.Kind == ast.ExprAddr || expr.Kind == ast.ExprDeref) {
		operand, ok := s.expressionType(*expr.Operand, substitutions)
		if ok {
			if expr.Kind == ast.ExprAddr {
				return ast.TypeExpr{Kind: ast.TypePointer, Elem: &operand, Span: expr.Span}, true
			}
			if operand.Kind == ast.TypePointer && operand.Elem != nil {
				return cloneGenericType(*operand.Elem), true
			}
		}
	}
	if expr.Type != nil && expr.Type.Kind != ast.TypeInvalid {
		typ := cloneGenericType(*expr.Type)
		s.rewriteType(&typ, substitutions)
		return typ, true
	}
	if expr.Kind == ast.ExprCall && expr.Callee != nil {
		if generated, ok := s.generatedFunc[genericCalleeName(*expr.Callee)]; ok && len(generated.Results) == 1 {
			return generated.Results[0].Type, true
		}
		name := genericCalleeName(*expr.Callee)
		if _, ok := s.typeDefs[name]; ok && len(expr.Args) == 1 {
			return ast.TypeExpr{Kind: ast.TypeName, Name: name, Span: expr.Span}, true
		}
	}
	// A rewritten operand can be more precise than the original semantic
	// fact, especially after a generic call has fixed a local's element type.
	if expr.Operand != nil && (expr.Kind == ast.ExprIndex || expr.Kind == ast.ExprSlice) {
		operand, ok := s.expressionType(*expr.Operand, substitutions)
		if ok {
			underlying := s.underlyingTypeExpr(operand, map[string]bool{})
			if underlying.Kind == ast.TypePointer && underlying.Elem != nil {
				underlying = s.underlyingTypeExpr(*underlying.Elem, map[string]bool{})
			}
			if expr.Kind == ast.ExprIndex {
				if underlying.Elem != nil && (underlying.Kind == ast.TypeArray || underlying.Kind == ast.TypeSlice || underlying.Kind == ast.TypeMap) {
					return cloneGenericType(*underlying.Elem), true
				}
				if underlying.Kind == ast.TypeName && canonicalGenericTypeName(underlying.Name) == "String" {
					return ast.TypeExpr{Kind: ast.TypeName, Name: "uint8", Span: expr.Span}, true
				}
			} else if underlying.Kind == ast.TypeSlice {
				return operand, true
			} else if underlying.Kind == ast.TypeArray && underlying.Elem != nil {
				return ast.TypeExpr{Kind: ast.TypeSlice, Elem: underlying.Elem, Span: expr.Span}, true
			} else if underlying.Kind == ast.TypeName && canonicalGenericTypeName(underlying.Name) == "String" {
				return operand, true
			}
		}
	}
	if s.activeAlias != "" {
		return ast.TypeExpr{}, false
	}
	info, ok := s.info.Exprs[expr.NodeID]
	if !ok || !info.Type.Valid() {
		return ast.TypeExpr{}, false
	}
	if info.Type.Kind == types.TypeParameter {
		for paramName, replacement := range substitutions {
			for _, object := range s.info.Objects {
				if object.Name == paramName && object.Type == info.Type {
					return replacement, true
				}
			}
		}
	}
	typ := s.sourceTypeExpr(info.Type, expr.Span)
	s.rewriteType(&typ, substitutions)
	return typ, true
}

func (s *genericSpecializer) assignmentTypes(values []ast.Expression, targets int, substitutions map[string]ast.TypeExpr) []ast.TypeExpr {
	if len(values) == 1 && values[0].Kind == ast.ExprCall && values[0].Callee != nil {
		if generated, ok := s.generatedFunc[genericCalleeName(*values[0].Callee)]; ok {
			results := make([]ast.TypeExpr, len(generated.Results))
			for i, field := range generated.Results {
				results[i] = cloneGenericType(field.Type)
			}
			return results
		}
	}
	results := make([]ast.TypeExpr, len(values))
	for i, value := range values {
		results[i], _ = s.expressionType(value, substitutions)
	}
	if len(values) == 1 && targets == 2 {
		value := values[0]
		if value.Kind == ast.ExprReceive || value.Kind == ast.ExprAssert || s.info.Exprs[value.NodeID].Category == check.ValueMapIndex {
			results = append(results, ast.TypeExpr{Kind: ast.TypeName, Name: "bool", Span: value.Span})
		}
	}
	return results
}

func (s *genericSpecializer) sourceTypeExpr(ref types.TypeRef, span source.Span) ast.TypeExpr {
	typ := semanticTypeExpr(s.info.TypeTable, ref, span)
	s.normalizeSourceTypeNames(&typ)
	return typ
}

func (s *genericSpecializer) normalizeSourceTypeNames(typ *ast.TypeExpr) {
	if typ == nil {
		return
	}
	if typ.Kind == ast.TypeName {
		name := typ.Name
		modulePrefix := strings.TrimSpace(s.info.ModulePath) + "."
		if s.info.ModulePath != "" && strings.HasPrefix(name, modulePrefix) {
			name = strings.TrimPrefix(name, modulePrefix)
		} else {
			aliases := make([]string, 0, len(s.importPaths))
			for alias := range s.importPaths {
				aliases = append(aliases, alias)
			}
			sort.Strings(aliases)
			for _, alias := range aliases {
				prefix := strings.TrimSpace(s.importPaths[alias]) + "."
				if prefix != "." && strings.HasPrefix(name, prefix) {
					name = alias + "." + strings.TrimPrefix(name, prefix)
					break
				}
			}
		}
		typ.Name = name
		return
	}
	s.normalizeSourceTypeNames(typ.Base)
	for i := range typ.TypeArgs {
		s.normalizeSourceTypeNames(&typ.TypeArgs[i])
	}
	s.normalizeSourceTypeNames(typ.Elem)
	s.normalizeSourceTypeNames(typ.Key)
	for i := range typ.Params {
		s.normalizeSourceTypeNames(&typ.Params[i].Type)
	}
	for i := range typ.Results {
		s.normalizeSourceTypeNames(&typ.Results[i].Type)
	}
	for i := range typ.Fields {
		s.normalizeSourceTypeNames(&typ.Fields[i].Type)
	}
	for i := range typ.Methods {
		for j := range typ.Methods[i].Params {
			s.normalizeSourceTypeNames(&typ.Methods[i].Params[j].Type)
		}
		for j := range typ.Methods[i].Results {
			s.normalizeSourceTypeNames(&typ.Methods[i].Results[j].Type)
		}
	}
	for i := range typ.Embeds {
		s.normalizeSourceTypeNames(&typ.Embeds[i])
	}
	for i := range typ.Terms {
		s.normalizeSourceTypeNames(&typ.Terms[i].Type)
	}
}

func semanticTypeExpr(table *types.TypeTable, ref types.TypeRef, span source.Span) ast.TypeExpr {
	if ref.Kind == types.Instance {
		node, ok := table.Node(ref)
		if ok {
			base := semanticTypeExpr(table, node.Base, span)
			args := make([]ast.TypeExpr, len(node.TypeArgs))
			for i := range node.TypeArgs {
				args[i] = semanticTypeExpr(table, node.TypeArgs[i], span)
			}
			return ast.TypeExpr{Kind: ast.TypeInstance, Base: &base, TypeArgs: args, Span: span}
		}
	}
	if ref.Kind == types.Named || ref.Kind == types.TypeParameter {
		text := types.FormatWithTable(table, ref)
		return ast.TypeExpr{Kind: ast.TypeName, Name: text, Span: span}
	}
	view := types.View(table, ref)
	switch view.Shape() {
	case types.Slice, types.Pointer:
		elemRef, _ := view.Elem()
		elem := semanticTypeExpr(table, elemRef, span)
		kind := ast.TypeSlice
		if view.Shape() == types.Pointer {
			kind = ast.TypePointer
		}
		return ast.TypeExpr{Kind: kind, Elem: &elem, Span: span}
	case types.Array:
		length, elemRef, _ := view.Array()
		elem := semanticTypeExpr(table, elemRef, span)
		literal := ast.Expression{Kind: ast.ExprLiteral, Literal: strconv.FormatInt(length, 10), Span: span}
		return ast.TypeExpr{Kind: ast.TypeArray, Elem: &elem, Len: &literal, Span: span}
	case types.Map:
		keyRef, elemRef, _ := view.Map()
		key := semanticTypeExpr(table, keyRef, span)
		elem := semanticTypeExpr(table, elemRef, span)
		return ast.TypeExpr{Kind: ast.TypeMap, Key: &key, Elem: &elem, Span: span}
	case types.Waitable:
		direction, elemRef, _ := view.Waitable()
		elem := semanticTypeExpr(table, elemRef, span)
		sourceDirection := ""
		switch direction {
		case types.ChannelReceive:
			sourceDirection = "recv"
		case types.ChannelSend:
			sourceDirection = "send"
		}
		return ast.TypeExpr{Kind: ast.TypeChan, Elem: &elem, Direction: sourceDirection, Span: span}
	case types.Function:
		signature, _ := view.Function()
		result := ast.TypeExpr{Kind: ast.TypeFunc, Span: span}
		for i := range signature.Params {
			result.Params = append(result.Params, ast.Field{Type: semanticTypeExpr(table, signature.Params[i].Type, span)})
		}
		if signature.Variadic && len(result.Params) != 0 {
			result.Params[len(result.Params)-1].Variadic = true
		}
		for _, item := range signature.Results {
			result.Results = append(result.Results, ast.Field{Type: semanticTypeExpr(table, item, span)})
		}
		return result
	case types.Struct:
		fields, _ := view.StructFields()
		result := ast.TypeExpr{Kind: ast.TypeStruct, Span: span}
		for _, field := range fields {
			result.Fields = append(result.Fields, ast.Field{Name: field.Name, Type: semanticTypeExpr(table, field.Type, span), Tag: field.Tag})
		}
		return result
	case types.Interface:
		methods, terms, _, _ := view.Interface()
		result := ast.TypeExpr{Kind: ast.TypeInterface, Span: span}
		for _, method := range methods {
			decl := ast.FuncDecl{Name: method.Name}
			for _, param := range method.Signature.Params {
				decl.Params = append(decl.Params, ast.Field{Type: semanticTypeExpr(table, param.Type, span)})
			}
			if method.Signature.Variadic && len(decl.Params) != 0 {
				decl.Params[len(decl.Params)-1].Variadic = true
			}
			for _, item := range method.Signature.Results {
				decl.Results = append(decl.Results, ast.Field{Type: semanticTypeExpr(table, item, span)})
			}
			result.Methods = append(result.Methods, decl)
		}
		for _, term := range terms {
			result.Terms = append(result.Terms, ast.TypeTerm{Type: semanticTypeExpr(table, term.Type, span), Approx: term.Approx})
		}
		return result
	}
	text := types.FormatWithTable(table, ref)
	return ast.TypeExpr{Kind: ast.TypeName, Name: text, Span: span}
}
