package semantic

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
)

type selectorCandidate struct {
	selection Selection
	fieldType types.TypeRef
}

type selectorFrontier struct {
	typ      types.TypeRef
	index    []int
	indirect bool
}

func (a *analyzer) finalizeSelector(expr *ast.Expression, info ExprInfo) ExprInfo {
	if expr.Operand == nil || strings.TrimSpace(expr.Field) == "" {
		return info
	}
	operand := a.info.Exprs[expr.Operand.NodeID]
	if operand.Mode == ExprPackage {
		object, ok := a.info.Object(operand.Object)
		if !ok {
			return info
		}
		export, ok := a.importedMember(object.ImportPath, expr.Operand.Name, expr.Field, expr.Span)
		if !ok {
			info.Type = types.TypeRef{}
			info.Mode = ExprInvalid
			return info
		}
		selection := Selection{
			Kind: SelectionPackageMember, Name: export.Name, ModulePath: export.ModulePath,
			FunctionID: export.ID, Type: a.dependencyType(export), Variadic: export.Variadic,
		}
		info.Type = selection.Type
		switch export.Kind {
		case ObjectType:
			info.Mode = ExprType
		case ObjectConst:
			info.Mode = ExprConstant
			info.Untyped = export.Untyped
		default:
			info.Mode = ExprValue
		}
		if export.Kind == ObjectVar {
			info.Category = ValueAddressable
		}
		if signature, ok := a.info.Relations.View(info.Type).Function(); ok {
			selection.Signature = signature
			selection.Variadic = selection.Variadic || signature.Variadic
			info.Signature = &signature
		}
		a.info.Selections[expr.NodeID] = selection
		return info
	}
	if !operand.Type.Valid() {
		return info
	}
	methodExpression := operand.Mode == ExprType
	candidate, ok := a.lookupSelector(operand.Type, expr.Field, methodExpression)
	if !ok {
		if methodExpression {
			a.addDiagnostic("semantic.method_expression.method", "method expression requires a visible method in receiver type method set", expr.Span)
			return info
		}
		if operand.Type.Kind == types.TypeParameter && a.typeParameterInScope(operand.Type, expr.NodeID) {
			constraint, known := a.info.Relations.View(operand.Type).Constraint()
			if constraint.Kind == types.Any {
				a.addDiagnostic("semantic.generic.selector", "type parameter constraint has no method "+expr.Field, expr.Span)
			} else if interfaceNode, interfaceOK := a.info.TypeTable.IsInterface(constraint); known && interfaceOK {
				found := false
				for _, method := range interfaceNode.Methods {
					found = found || method.Name == expr.Field
				}
				if !found {
					a.addDiagnostic("semantic.generic.selector", "type parameter constraint has no method "+expr.Field, expr.Span)
				}
			}
		}
		return info
	}
	selection := candidate.selection
	if methodExpression {
		selection.Kind = SelectionMethodExpression
		signature := selection.Signature
		signature.Params = append([]types.TypeParam{{Type: selection.Receiver}}, signature.Params...)
		selection.Type = a.storeFunctionType(expr.NodeID, "selection", signature)
		selection.Signature = signature
		info.Type = selection.Type
		info.Signature = &signature
		info.Mode = ExprValue
	} else if selection.Kind == SelectionMethod {
		selection.Type = a.storeFunctionType(expr.NodeID, "selection", selection.Signature)
		info.Type = selection.Type
		signature := selection.Signature
		info.Signature = &signature
		info.Mode = ExprValue
	} else {
		selection.Type = candidate.fieldType
		info.Type = candidate.fieldType
		info.Mode = ExprValue
		if operand.Category.Addressable() || a.info.Relations.View(operand.Type).Shape() == types.Pointer {
			info.Category = ValueAddressable
		}
		if signature, ok := a.info.Relations.View(candidate.fieldType).Function(); ok {
			info.Signature = &signature
		}
	}
	a.info.Selections[expr.NodeID] = selection
	return info
}

func (a *analyzer) lookupSelector(receiver types.TypeRef, name string, methodExpression bool) (selectorCandidate, bool) {
	receiver = a.resolveAlias(receiver)
	frontier := []selectorFrontier{{typ: receiver}}
	seen := make(map[string]struct{})
	for len(frontier) != 0 {
		var candidates []selectorCandidate
		var next []selectorFrontier
		for _, item := range frontier {
			key := types.FormatWithTable(a.info.TypeTable, item.typ)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, a.directSelectorCandidates(item, name, methodExpression)...)
			for index, field := range a.structFields(item.typ) {
				if !field.Embedded {
					continue
				}
				path := append(append([]int(nil), item.index...), index)
				next = append(next, selectorFrontier{
					typ: field.Type, index: path,
					indirect: item.indirect || a.info.Relations.View(item.typ).Shape() == types.Pointer,
				})
			}
		}
		if len(candidates) == 1 {
			candidates[0].selection.Receiver = receiver
			return candidates[0], true
		}
		if len(candidates) > 1 {
			return selectorCandidate{}, false
		}
		frontier = next
	}
	return selectorCandidate{}, false
}

func (a *analyzer) resolveAlias(ref types.TypeRef) types.TypeRef {
	seen := make(map[types.TypeID]struct{})
	for {
		node, ok := a.typeNode(ref)
		if !ok || node.Kind != types.Named || !node.Alias || !node.AliasTarget.Valid() {
			return ref
		}
		if _, exists := seen[node.ID]; exists {
			return ref
		}
		seen[node.ID] = struct{}{}
		ref = node.AliasTarget
	}
}

func (a *analyzer) directSelectorCandidates(item selectorFrontier, name string, methodExpression bool) []selectorCandidate {
	var out []selectorCandidate
	if !methodExpression {
		for index, field := range a.structFields(item.typ) {
			if field.Name == name && a.visibleMember(name, a.typeModule(item.typ)) {
				path := append(append([]int(nil), item.index...), index)
				out = append(out, selectorCandidate{
					selection: Selection{Kind: SelectionField, Name: name, Receiver: item.typ, Type: field.Type, Index: path},
					fieldType: field.Type,
				})
			}
		}
	}
	for _, method := range a.directMethods(item.typ, methodExpression && !item.indirect) {
		modulePath := strings.TrimSpace(method.ModulePath)
		if modulePath == "" {
			modulePath = a.typeModule(item.typ)
		}
		if method.Name != name || !a.visibleMember(name, modulePath) {
			continue
		}
		declaringReceiver := method.Receiver
		if !declaringReceiver.Valid() {
			declaringReceiver = item.typ
		}
		out = append(out, selectorCandidate{selection: Selection{
			Kind: SelectionMethod, Name: name, ModulePath: modulePath,
			FunctionID: method.FunctionID, Receiver: item.typ, DeclaringReceiver: declaringReceiver,
			Signature: method.Signature, Variadic: method.Signature.Variadic,
			Interface: a.isInterface(item.typ), Indirect: item.typ.Kind != types.Pointer && method.Receiver.Kind == types.Pointer,
			Index: append([]int(nil), item.index...),
		}})
	}
	return out
}

func (a *analyzer) structFields(ref types.TypeRef) []types.Field {
	for a.info.Relations.View(ref).Shape() == types.Pointer {
		elem, ok := a.info.Relations.View(ref).Elem()
		if !ok {
			return nil
		}
		ref = elem
	}
	fields, ok := a.info.Relations.View(ref).StructFields()
	if !ok {
		return nil
	}
	return fields
}

func (a *analyzer) directMethods(ref types.TypeRef, methodExpression bool) []types.Method {
	if constraint, ok := a.info.Relations.View(ref).Constraint(); ok {
		if interfaceNode, interfaceOK := a.info.TypeTable.IsInterface(constraint); interfaceOK {
			return append([]types.Method(nil), interfaceNode.Methods...)
		}
	}
	if interfaceNode, ok := a.info.TypeTable.IsInterface(ref); ok {
		if namedNode, named := a.typeNode(ref); named && namedNode.Kind == types.Named && len(namedNode.Methods) != 0 {
			return append([]types.Method(nil), namedNode.Methods...)
		}
		return append([]types.Method(nil), interfaceNode.Methods...)
	}
	pointer := a.info.Relations.View(ref).Shape() == types.Pointer
	base := ref
	if pointer {
		base, _ = a.info.Relations.View(ref).Elem()
	}
	node, ok := a.typeNode(a.resolveAlias(base))
	if !ok || node.Kind != types.Named {
		return nil
	}
	var out []types.Method
	for _, method := range node.Methods {
		methodPointer := a.info.Relations.View(method.Receiver).Shape() == types.Pointer
		declaring := method.Receiver
		if methodPointer {
			declaring, _ = a.info.Relations.View(declaring).Elem()
		}
		// Imported method sets include promoted methods. Their receiver still
		// belongs to the embedded field, which selector traversal must visit.
		if declaring.Valid() && !a.info.Relations.Identical(a.resolveAlias(base), a.resolveAlias(declaring)).OK {
			continue
		}
		if methodExpression && methodPointer && !pointer {
			continue
		}
		out = append(out, method)
	}
	return out
}

func (a *analyzer) isInterface(ref types.TypeRef) bool {
	_, ok := a.info.TypeTable.IsInterface(ref)
	return ok
}

func (a *analyzer) typeModule(ref types.TypeRef) string {
	if a.info.Relations.View(ref).Shape() == types.Pointer {
		ref, _ = a.info.Relations.View(ref).Elem()
	}
	if node, ok := a.typeNode(ref); ok && node.Kind == types.Named {
		return node.Identity.ModulePath
	}
	return a.info.ModulePath
}

func (a *analyzer) typeNode(ref types.TypeRef) (types.TypeNode, bool) {
	if ref.Kind == types.Named && ref.Node == "" {
		return a.info.TypeTable.Named(ref.Named)
	}
	return a.info.TypeTable.Node(ref)
}

func (a *analyzer) visibleMember(name, modulePath string) bool {
	return isExported(name) || strings.TrimSpace(modulePath) == strings.TrimSpace(a.info.ModulePath)
}
