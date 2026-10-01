package semantic

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (a *analyzer) validateTypeSetConstraint(constraint ast.TypeExpr) {
	if constraint.Kind != ast.TypeInterface || len(constraint.Terms) == 0 {
		return
	}
	type checkedTerm struct {
		ref    types.TypeRef
		approx bool
	}
	checked := make([]checkedTerm, 0, len(constraint.Terms))
	for _, term := range constraint.Terms {
		ref := a.info.Relations.ResolveAlias(a.resolvedType(term.Type))
		if !ref.Valid() {
			continue
		}
		view := a.info.Relations.View(ref)
		if view.Shape() == types.Interface {
			a.addDiagnostic("semantic.type_set.term.interface", "interface type cannot be used as a union term", term.Span)
			continue
		}
		if term.Approx && (ref.Kind == types.TypeParameter || ref.Kind == types.Named && ref != view.Underlying()) {
			a.addDiagnostic("semantic.type_set.term.approx", "approximation term must name its own underlying type", term.Span)
			continue
		}
		duplicate := false
		for _, previous := range checked {
			if term.Approx == previous.approx && a.info.Relations.Identical(ref, previous.ref).OK {
				a.addDiagnostic("semantic.type_set.term.duplicate", "type set contains a duplicate term", term.Span)
				duplicate = true
				break
			}
			if (term.Approx || previous.approx) && a.info.Relations.UnderlyingIdentical(ref, previous.ref).OK {
				a.addDiagnostic("semantic.type_set.term.overlap", "type set contains overlapping terms", term.Span)
				duplicate = true
				break
			}
		}
		if !duplicate {
			checked = append(checked, checkedTerm{ref: ref, approx: term.Approx})
		}
	}
}

func (a *analyzer) deferTypeParameterCheck(ref types.TypeRef) bool {
	constraint, ok := a.info.Relations.View(ref).Constraint()
	if !ok || constraint.Kind != types.Instance {
		return false
	}
	_, resolved := a.instanceTypeSetTerms(constraint)
	return !resolved
}

func (a *analyzer) typeParameterTerms(ref types.TypeRef) ([]types.TypeRef, bool) {
	constraint, ok := a.info.Relations.View(ref).Constraint()
	if !ok || constraint.Kind == types.Any {
		return nil, false
	}
	if constraint.Kind == types.Instance {
		return a.instanceTypeSetTerms(constraint)
	}
	if terms, ok := a.info.Relations.View(constraint).TypeSetTerms(); ok {
		out := make([]types.TypeRef, 0, len(terms))
		for _, term := range terms {
			if !term.Type.Valid() {
				return nil, false
			}
			out = append(out, term.Type)
		}
		return out, len(out) != 0
	}
	return []types.TypeRef{constraint}, true
}

func (a *analyzer) instanceTypeSetTerms(ref types.TypeRef) ([]types.TypeRef, bool) {
	instance, ok := a.info.TypeTable.Node(ref)
	if !ok || instance.Kind != types.Instance {
		return nil, false
	}
	bindings, ok := a.instanceTypeBindings(instance)
	if !ok {
		return nil, false
	}
	terms, ok := a.info.Relations.View(instance.Base).TypeSetTerms()
	if !ok {
		return nil, false
	}
	out := make([]types.TypeRef, 0, len(terms))
	cache := make(map[types.TypeID]types.TypeRef)
	for _, term := range terms {
		resolved, valid := a.substituteType(term.Type, bindings, "constraint."+string(instance.ID), cache)
		if !valid {
			return nil, false
		}
		out = append(out, resolved)
	}
	return out, len(out) != 0
}

func (a *analyzer) instanceTypeBindings(instance types.TypeNode) (map[types.TypeRef]types.TypeRef, bool) {
	bindings := make(map[types.TypeRef]types.TypeRef, len(instance.TypeArgs))
	if export, ok := a.dependency(instance.Base.Named.ModulePath, string(instance.Base.Named.DeclID)); ok && len(export.TypeParams) == len(instance.TypeArgs) {
		parser := a.dependencyParser(export)
		for i, name := range export.TypeParams {
			bindings[parser.Bindings[name]] = instance.TypeArgs[i]
		}
		return bindings, true
	}
	var params []ObjectID
	for objectID, candidates := range a.info.GenericDecls {
		object, exists := a.info.Object(objectID)
		if exists && object.Type == instance.Base {
			params = candidates
			break
		}
	}
	if len(params) == 0 || len(params) != len(instance.TypeArgs) {
		return nil, false
	}
	for i, objectID := range params {
		object, exists := a.info.Object(objectID)
		if !exists || !object.Type.Valid() || !instance.TypeArgs[i].Valid() {
			return nil, false
		}
		bindings[object.Type] = instance.TypeArgs[i]
	}
	return bindings, true
}

func (a *analyzer) substituteType(ref types.TypeRef, bindings map[types.TypeRef]types.TypeRef, prefix string, cache map[types.TypeID]types.TypeRef) (types.TypeRef, bool) {
	if replacement, ok := bindings[ref]; ok {
		return replacement, true
	}
	if ref.Node == "" || ref.Kind == types.Named {
		return ref, ref.Valid()
	}
	if replacement, ok := cache[ref.Node]; ok {
		return replacement, true
	}
	node, ok := a.info.TypeTable.Node(ref)
	if !ok || node.Kind == types.TypeParameter {
		return ref, ok && ref.Valid()
	}
	switch node.Kind {
	case types.Slice, types.Array, types.Map, types.Pointer, types.Waitable, types.Function, types.Tuple, types.Struct, types.Interface, types.Instance:
	default:
		return ref, ref.Valid()
	}
	node.ID = types.TypeID(prefix + "." + string(node.ID))
	resolved := types.TypeRef{Kind: node.Kind, Node: node.ID}
	cache[ref.Node] = resolved
	node.Tuple = append([]types.TypeRef(nil), node.Tuple...)
	node.Fields = append([]types.Field(nil), node.Fields...)
	node.TypeArgs = append([]types.TypeRef(nil), node.TypeArgs...)
	resolve := func(current types.TypeRef) (types.TypeRef, bool) {
		if !current.Valid() {
			return current, true
		}
		return a.substituteType(current, bindings, prefix, cache)
	}
	if node.Elem, ok = resolve(node.Elem); !ok {
		return types.TypeRef{}, false
	}
	if node.Key, ok = resolve(node.Key); !ok {
		return types.TypeRef{}, false
	}
	for i := range node.Tuple {
		if node.Tuple[i], ok = resolve(node.Tuple[i]); !ok {
			return types.TypeRef{}, false
		}
	}
	for i := range node.Fields {
		if node.Fields[i].Type, ok = resolve(node.Fields[i].Type); !ok {
			return types.TypeRef{}, false
		}
	}
	for i := range node.TypeArgs {
		if node.TypeArgs[i], ok = resolve(node.TypeArgs[i]); !ok {
			return types.TypeRef{}, false
		}
	}
	node.Terms = append([]types.TypeTerm(nil), node.Terms...)
	for i := range node.Terms {
		if node.Terms[i].Type, ok = resolve(node.Terms[i].Type); !ok {
			return types.TypeRef{}, false
		}
	}
	if node.Signature != nil {
		signature := *node.Signature
		signature.Params = append([]types.TypeParam(nil), signature.Params...)
		signature.Results = append([]types.TypeRef(nil), signature.Results...)
		for i := range signature.Params {
			if signature.Params[i].Type, ok = resolve(signature.Params[i].Type); !ok {
				return types.TypeRef{}, false
			}
		}
		for i := range signature.Results {
			if signature.Results[i], ok = resolve(signature.Results[i]); !ok {
				return types.TypeRef{}, false
			}
		}
		node.Signature = &signature
	}
	if _, exists := a.info.TypeTable.Node(resolved); exists {
		_ = a.info.TypeTable.Replace(node)
	} else if err := a.info.TypeTable.Add(node); err != nil {
		return types.TypeRef{}, false
	}
	return resolved, true
}
