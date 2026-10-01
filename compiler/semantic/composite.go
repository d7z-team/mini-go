package semantic

import (
	"strconv"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (a *analyzer) compositeInfo(expr *ast.Expression) CompositeInfo {
	ref := a.resolvedTypePtr(expr.Type)
	if !ref.Valid() {
		ref = a.info.Composites[expr.NodeID].Type
	}
	info := CompositeInfo{Type: a.resolveAlias(ref), TypeExact: true}
	if expr.Type != nil && expr.Type.Kind == ast.TypeArray && expr.Type.LenInfer {
		info.Shape = types.Array
		info.InferredArray = true
		if expr.Type.Elem != nil {
			info.Element = a.resolvedType(*expr.Type.Elem)
		}
		length, ok := a.constantArrayOperandLength(*expr, a.info.NodeScopes[expr.NodeID])
		if !ok || !info.Element.Valid() {
			info.TypeExact = false
			return info
		}
		id := types.TypeID("inferred_array." + strconv.FormatUint(uint64(expr.NodeID), 10))
		ref := types.TypeRef{Kind: types.Array, Node: id}
		_ = a.info.TypeTable.Add(types.TypeNode{ID: id, Kind: types.Array, Elem: info.Element, Length: length})
		info.Type = ref
		return info
	}
	if expr.Type != nil && expr.Type.Kind == ast.TypeArray && expr.Type.Len != nil {
		_, info.TypeExact = a.info.ArrayLengths[expr.Type.Len.NodeID]
	}
	if !info.Type.Valid() {
		return info
	}
	view := a.info.Relations.View(info.Type)
	if node, ok := a.info.TypeTable.Node(info.Type); ok && node.Kind == types.Instance {
		view = a.info.Relations.View(node.Base)
		info.TypeExact = false
	}
	info.Shape = view.Shape()
	switch info.Shape {
	case types.Array:
		_, info.Element, _ = view.Array()
	case types.Slice:
		info.Element, _ = view.Elem()
	case types.Map:
		info.Key, info.Element, _ = view.Map()
	case types.Struct:
		fields, _ := view.StructFields()
		for _, field := range fields {
			signature, isFunction := a.info.Relations.View(field.Type).Function()
			info.Fields = append(info.Fields, CompositeField{
				Name: field.Name, Type: field.Type, Tag: field.Tag, Embedded: field.Embedded,
				Variadic: isFunction && signature.Variadic,
			})
		}
		info.ModulePath = a.typeModule(a.resolveAlias(info.Type))
	}
	return info
}

func (a *analyzer) analyzeCompositeElements(expr *ast.Expression, scope ScopeID) {
	composite := a.compositeInfo(expr)
	composite.Initializers = make([]Selection, len(expr.Items))
	a.info.Composites[expr.NodeID] = composite
	analyzeValue := func(value *ast.Expression, target types.TypeRef) {
		if value.Kind == ast.ExprComposite && (value.Type == nil || value.Type.Kind == ast.TypeInvalid) && target.Valid() {
			if view := a.info.Relations.View(target); view.Shape() == types.Pointer {
				target, _ = view.Elem()
			}
			a.info.Composites[value.NodeID] = CompositeInfo{Type: target}
		}
		a.analyzeExpr(value, scope)
		if target.Valid() {
			a.validateAssignments([]ast.Expression{*value}, []types.TypeRef{target})
		}
	}
	for i := range expr.Items {
		entry := &expr.Items[i]
		target := composite.Element
		if composite.Shape == types.Struct {
			if entry.Key == nil && i < len(composite.Fields) {
				target = composite.Fields[i].Type
			} else if entry.Key != nil && entry.Key.Kind == ast.ExprIdent {
				candidate, found := a.lookupSelector(composite.Type, entry.Key.Name, false)
				if candidate.ambiguous {
					a.addDiagnostic("semantic.composite.ambiguous", "struct initializer names an ambiguous promoted field", entry.Key.Span)
				} else if candidate.inaccessible {
					a.addDiagnostic("semantic.composite.unexported", "struct initializer names an inaccessible field", entry.Key.Span)
				}
				if found && candidate.selection.Kind == SelectionField {
					selection := candidate.selection
					target = candidate.fieldType
					composite.Initializers[i] = selection
					a.info.Selections[entry.Key.NodeID] = selection
					current := composite.Type
					for depth, index := range selection.Index {
						if depth > 0 && a.info.Relations.View(current).Shape() == types.Pointer {
							a.addDiagnostic("semantic.composite.embedded_pointer", "cannot initialize a promoted field through an embedded pointer", entry.Key.Span)
							break
						}
						current = a.structFields(current)[index].Type
					}
					for _, previous := range composite.Initializers[:i] {
						if len(previous.Index) == 0 {
							continue
						}
						prefix := true
						for j := 0; j < len(previous.Index) && j < len(selection.Index); j++ {
							prefix = prefix && previous.Index[j] == selection.Index[j]
						}
						if prefix && len(previous.Index) != len(selection.Index) {
							a.addDiagnostic("semantic.composite.overlap", "struct initializers overlap an embedded field", entry.Key.Span)
						}
					}
				}
			}
		} else if entry.Key != nil && composite.Type.Valid() {
			analyzeValue(entry.Key, composite.Key)
		}
		analyzeValue(&entry.Value, target)
	}
	a.info.Composites[expr.NodeID] = composite
}
