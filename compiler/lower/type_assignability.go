package lower

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) typeDeclFor(typ string) (ast.TypeExpr, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return ast.TypeExpr{}, false
	}
	if decl, ok := l.typeDecls[typ]; ok {
		return decl, true
	}
	if prefix := strings.TrimSpace(l.modulePath) + "."; prefix != "." && strings.HasPrefix(typ, prefix) {
		if decl, ok := l.typeDecls[strings.TrimPrefix(typ, prefix)]; ok {
			return decl, true
		}
	}
	resolved := l.resolveType(typ)
	if resolved != typ {
		if decl, ok := l.typeDecls[resolved]; ok {
			return decl, true
		}
	}
	return ast.TypeExpr{}, false
}

func (l *lowerer) interfaceType(typ string) (string, bool) {
	typ = l.resolveType(strings.TrimSpace(typ))
	view, ok := l.typeView(typ)
	if !ok || view.Shape() != types.Interface {
		return "", false
	}
	return l.typeRefString(view.Underlying()), true
}

func (l *lowerer) isGeneralInterfaceType(typ string) bool {
	canonical, ok := l.interfaceType(typ)
	if !ok {
		return false
	}
	return l.interfaceHasTypeSet(canonical, map[string]struct{}{})
}

func (l *lowerer) interfaceHasTypeSet(canonical string, seen map[string]struct{}) bool {
	canonical, ok := l.interfaceType(canonical)
	if !ok {
		return false
	}
	if canonical == "" {
		return false
	}
	if _, recursive := seen[canonical]; recursive {
		return false
	}
	seen[canonical] = struct{}{}
	defer delete(seen, canonical)
	view, ok := l.typeView(canonical)
	if !ok {
		return false
	}
	_, terms, hasTypeSet, ok := view.Interface()
	if !ok {
		return false
	}
	if hasTypeSet {
		return true
	}
	for _, term := range terms {
		if term.Approx || term.Union {
			return true
		}
		member := l.typeRefString(term.Type)
		if member == "Any" {
			continue
		}
		if _, ok := l.interfaceType(member); ok {
			if l.interfaceHasTypeSet(member, seen) {
				return true
			}
			continue
		}
		// A bare member that is not an interface reference is a singleton
		// type term. The parser cannot classify it before declarations/imports
		// are known, so classification belongs here.
		return true
	}
	return false
}

func (l *lowerer) interfaceMethods(typ string, seen map[string]struct{}) map[string]string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return nil
	}
	if _, recursive := seen[typ]; recursive {
		return nil
	}
	seen[typ] = struct{}{}
	defer delete(seen, typ)
	out := map[string]string{}
	if decl, ok := l.typeDeclFor(typ); ok && decl.Kind == ast.TypeInterface {
		for _, method := range l.interfaceTypeMethodMetadata(typ, decl, map[string]struct{}{}) {
			signature := l.functionIdentitySignature(method.Signature, method.Variadic)
			if signature != "" {
				owner := strings.TrimSpace(method.ModulePath)
				if owner == "" {
					owner = l.currentModulePath()
				}
				out[methodIdentity(owner, method.Name)] = signature
			}
		}
		return out
	}
	if export, ok := l.importedTypeInfo(typ); ok {
		if _, ok := l.interfaceType(export.Underlying); !ok {
			return nil
		}
		for _, method := range export.Methods {
			name := strings.TrimSpace(method.Name)
			if name == "" || isBlankIdentifier(name) {
				continue
			}
			signature := l.functionIdentitySignature(method.Signature, method.Variadic)
			if signature != "" {
				owner := strings.TrimSpace(method.ModulePath)
				if owner == "" {
					owner = strings.TrimSpace(export.ModulePath)
				}
				out[methodIdentity(owner, name)] = signature
			}
		}
		return out
	}
	canonical, ok := l.interfaceType(typ)
	if !ok {
		return nil
	}
	if canonical != typ {
		if _, recursive := seen[canonical]; recursive {
			return nil
		}
		seen[canonical] = struct{}{}
		defer delete(seen, canonical)
	}
	view, ok := l.typeView(canonical)
	if !ok {
		return out
	}
	methods, terms, _, ok := view.Interface()
	if !ok {
		return out
	}
	for _, method := range methods {
		name := strings.TrimSpace(method.Name)
		if name == "" || isBlankIdentifier(name) {
			continue
		}
		owner := strings.TrimSpace(method.ModulePath)
		if owner == "" {
			owner = l.currentModulePath()
		}
		out[methodIdentity(owner, name)] = l.formatFunctionSignature(method.Signature, map[types.TypeID]bool{})
	}
	for _, term := range terms {
		if term.Approx || term.Union {
			continue
		}
		member := l.typeRefString(term.Type)
		if _, ok := l.interfaceType(member); !ok {
			continue
		}
		for identity, signature := range l.interfaceMethods(member, seen) {
			out[identity] = signature
		}
	}
	return out
}

func (l *lowerer) currentModulePath() string {
	if l == nil {
		return ""
	}
	if l.modulePath != "" {
		return l.modulePath
	}
	if l.program == nil {
		return ""
	}
	return strings.TrimSpace(l.program.ModulePath)
}

func methodIdentity(owner, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if isExported(name) {
		return name
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return name
	}
	return owner + "." + name
}

func methodNameFromIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	if isExported(identity) {
		return identity
	}
	dot := strings.LastIndex(identity, ".")
	if dot >= 0 && dot < len(identity)-1 {
		return strings.TrimSpace(identity[dot+1:])
	}
	return identity
}

func methodOwnerFromIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" || isExported(identity) {
		return ""
	}
	dot := strings.LastIndex(identity, ".")
	if dot <= 0 {
		return ""
	}
	return strings.TrimSpace(identity[:dot])
}

func (l *lowerer) assignableType(source, target string) bool {
	return l.assignmentRelation(source, target).OK
}

func (l *lowerer) assignmentRelation(source, target string) types.RelationResult {
	source = l.resolveType(strings.TrimSpace(source))
	target = l.resolveType(strings.TrimSpace(target))
	if source == "" || target == "" {
		return types.RelationResult{OK: true, Code: types.RelationOK}
	}
	sourceRef, sourceOK := l.typeRef(source)
	targetRef, targetOK := l.typeRef(target)
	if !sourceOK || !targetOK {
		return types.RelationResult{Code: types.RelationInvalid}
	}
	return l.typeRelations().Assignable(sourceRef, targetRef)
}

func (l *lowerer) assignmentRelationRef(source types.TypeRef, target string) (types.RelationResult, bool) {
	targetRef, ok := l.typeRef(target)
	if !source.Valid() || !ok {
		return types.RelationResult{Code: types.RelationInvalid}, false
	}
	return l.typeRelations().Assignable(source, targetRef), true
}

func (l *lowerer) semanticAssignmentRelation(expr ast.Expression, result int, target string) (types.RelationResult, bool) {
	if l.semantic == nil || expr.NodeID == 0 {
		return types.RelationResult{}, false
	}
	info, ok := l.semantic.Exprs[expr.NodeID]
	if !ok {
		return types.RelationResult{}, false
	}
	source := info.Type
	if result >= 0 {
		if result >= len(info.Results) {
			return types.RelationResult{}, false
		}
		source = info.Results[result]
	}
	if !l.semantic.TypeExact(source) {
		return types.RelationResult{}, false
	}
	return l.assignmentRelationRef(source, target)
}

func (l *lowerer) isFunctionSignatureType(typ string) bool {
	_, _, _, ok := l.functionSignatureParts(typ)
	return ok
}

func (l *lowerer) functionSignatureParts(signature string) ([]string, []string, bool, bool) {
	view, ok := l.typeView(signature)
	if !ok || view.Shape() != types.Function {
		return nil, nil, false, false
	}
	function, ok := view.Function()
	if !ok {
		return nil, nil, false, false
	}
	params := make([]string, len(function.Params))
	for i, param := range function.Params {
		params[i] = l.typeRefString(param.Type)
	}
	results := make([]string, len(function.Results))
	for i, result := range function.Results {
		results[i] = l.typeRefString(result)
	}
	return params, results, function.Variadic, true
}

func (l *lowerer) implementsInterfaceType(source, target string) bool {
	sourceRef, sourceOK := l.typeRef(source)
	targetRef, targetOK := l.typeRef(target)
	return sourceOK && targetOK && l.typeRelations().Implements(sourceRef, targetRef).OK
}
