package specialize

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (s *genericSpecializer) validateTypeArgs(name string, generic genericDecl, args []ast.TypeExpr, span source.Span) bool {
	params := generic.typeParams
	if len(params) != len(args) {
		s.addDiagnostic("compiler.generic.arity", fmt.Sprintf("%s requires %d type arguments, got %d", name, len(params), len(args)), span)
		return false
	}
	valid := true
	bindings := bindTypeArgs(params, args)
	for i := range params {
		constraint := cloneGenericType(params[i].Constraint)
		s.qualifyImportedType(&constraint, generic.importAlias)
		substituteGenericType(&constraint, bindings)
		if !s.satisfiesConstraint(args[i], constraint, generic.namedTypes, map[string]bool{}) {
			s.addDiagnostic("compiler.generic.constraint", fmt.Sprintf("type argument %s does not satisfy constraint for %s", genericTypeText(args[i]), params[i].Name), args[i].Span)
			valid = false
		}
	}
	return valid
}

func (s *genericSpecializer) satisfiesConstraint(arg, constraint ast.TypeExpr, namedTypes map[string]ast.TypeExpr, seen map[string]bool) bool {
	name := constraint.Name
	if constraint.Kind == ast.TypeName {
		if name == "any" || name == "Any" {
			return true
		}
		if name == "comparable" {
			ref := s.sourceTypeRef(arg)
			if ref.Valid() {
				return s.info.Relations.Comparable(ref).OK
			}
			return genericTypeComparable(arg, s.typeDefs, map[string]bool{})
		}
		if seen[name] {
			return false
		}
		resolved, ok := namedTypes[name]
		if !ok {
			resolved, ok = s.typeDefs[name]
		}
		if !ok {
			return false
		}
		seen[name] = true
		ok = s.satisfiesConstraint(arg, resolved, namedTypes, seen)
		delete(seen, name)
		return ok
	}
	if constraint.Kind == ast.TypeInstance && constraint.Base != nil {
		name := genericTypeText(*constraint.Base)
		generic, ok := s.types[name]
		if !ok || len(generic.typeParams) != len(constraint.TypeArgs) {
			return false
		}
		resolved := cloneGenericType(generic.decl.Type.Type)
		substituteGenericType(&resolved, bindTypeArgs(generic.typeParams, constraint.TypeArgs))
		return s.satisfiesConstraint(arg, resolved, generic.namedTypes, seen)
	}
	if constraint.Kind != ast.TypeInterface {
		return genericTypeText(arg) == genericTypeText(constraint)
	}
	if len(constraint.Terms) != 0 {
		argument := genericTypeText(arg)
		underlying := genericTypeText(s.underlyingTypeExpr(arg, map[string]bool{}))
		matched := false
		for _, term := range constraint.Terms {
			termText := genericTypeText(term.Type)
			if !term.Approx && termText == argument || term.Approx && termText == underlying {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, embedded := range constraint.Embeds {
		if !s.satisfiesConstraint(arg, embedded, namedTypes, seen) {
			return false
		}
	}
	for _, required := range constraint.Methods {
		if !s.hasConstraintMethod(arg, required) {
			return false
		}
	}
	return true
}

func (s *genericSpecializer) registerMethod(receiver string, method ast.FuncDecl, pointer bool) {
	receiver = strings.TrimSpace(receiver)
	if receiver == "" || method.Name == "" || len(method.TypeParams) != 0 {
		return
	}
	keys := []string{receiver}
	if pointer {
		keys = []string{"Ptr<" + receiver + ">"}
	} else {
		keys = append(keys, "Ptr<"+receiver+">")
	}
	for _, key := range keys {
		if s.methodSets[key] == nil {
			s.methodSets[key] = make(map[string]genericMethod)
		}
		s.methodSets[key][method.Name] = genericMethod{decl: method, pointer: pointer}
	}
}

func (s *genericSpecializer) hasConstraintMethod(arg ast.TypeExpr, required ast.FuncDecl) bool {
	if definition, ok := s.typeDefs[genericTypeText(arg)]; ok && definition.Kind == ast.TypeInterface {
		for _, method := range definition.Methods {
			if method.Name == required.Name && sameGenericSignature(method, required) {
				return true
			}
		}
	}
	methods := s.methodSets[genericTypeText(arg)]
	method, ok := methods[required.Name]
	if !ok {
		return false
	}
	return sameGenericSignature(method.decl, required)
}

func sameGenericSignature(actual, required ast.FuncDecl) bool {
	if len(actual.Params) != len(required.Params) || len(actual.Results) != len(required.Results) {
		return false
	}
	variadic := false
	requiredVariadic := false
	for i := range required.Params {
		if genericTypeText(actual.Params[i].Type) != genericTypeText(required.Params[i].Type) {
			return false
		}
		variadic = variadic || actual.Params[i].Variadic
		requiredVariadic = requiredVariadic || required.Params[i].Variadic
	}
	if variadic != requiredVariadic {
		return false
	}
	for i := range required.Results {
		if genericTypeText(actual.Results[i].Type) != genericTypeText(required.Results[i].Type) {
			return false
		}
	}
	return true
}

func genericTypeComparable(typ ast.TypeExpr, definitions map[string]ast.TypeExpr, seen map[string]bool) bool {
	name := genericTypeText(typ)
	if definition, ok := definitions[name]; ok && !seen[name] {
		seen[name] = true
		return genericTypeComparable(definition, definitions, seen)
	}
	switch typ.Kind {
	case ast.TypeSlice, ast.TypeMap, ast.TypeFunc:
		return false
	case ast.TypeArray, ast.TypePointer, ast.TypeChan:
		return typ.Elem == nil || genericTypeComparable(*typ.Elem, definitions, seen)
	case ast.TypeStruct:
		for _, field := range typ.Fields {
			if !genericTypeComparable(field.Type, definitions, seen) {
				return false
			}
		}
	}
	return true
}

func (s *genericSpecializer) sourceTypeRef(typ ast.TypeExpr) types.TypeRef {
	if info, ok := s.info.Types[typ.NodeID]; ok {
		return info.Type
	}
	return types.TypeRef{}
}
