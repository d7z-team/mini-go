package semantic

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
)

type selectorCandidate struct {
	selection    Selection
	fieldType    types.TypeRef
	ambiguous    bool
	inaccessible bool
}

type selectorFrontier struct {
	typ      types.TypeRef
	index    []int
	indirect bool
	multiple bool
	key      string
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
	info.Object = selection.Object
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
	lookupKey := func(ref types.TypeRef, indirect bool) string {
		ref = a.resolveAlias(ref)
		pointer := a.info.Relations.View(ref).Shape() == types.Pointer
		if pointer {
			ref, _ = a.info.Relations.View(ref).Elem()
			ref = a.resolveAlias(ref)
		}
		// Member names belong to the declaration. Recursive instantiations
		// must not expand that declaration again for each changing argument.
		if node, ok := a.typeNode(ref); ok && node.Kind == types.Instance {
			ref = a.resolveAlias(node.Base)
		}
		key := types.FormatWithTable(a.info.TypeTable, ref)
		if pointer {
			key = "Ptr<" + key + ">"
		}
		if indirect {
			key += "\x00indirect"
		}
		return key
	}
	frontier := []selectorFrontier{{typ: receiver, key: lookupKey(receiver, false)}}
	seen := make(map[string]bool)
	for len(frontier) != 0 {
		var candidates []selectorCandidate
		var next []selectorFrontier
		nextIndex := make(map[string]int)
		for _, item := range frontier {
			if seen[item.key] {
				continue
			}
			seen[item.key] = true
			direct := a.directSelectorCandidates(item, name, methodExpression)
			if item.multiple && len(direct) != 0 {
				return selectorCandidate{ambiguous: true}, false
			}
			candidates = append(candidates, direct...)
			for index, field := range a.structFields(item.typ) {
				if !field.Embedded {
					continue
				}
				indirect := item.indirect || a.info.Relations.View(item.typ).Shape() == types.Pointer
				key := lookupKey(field.Type, indirect)
				if seen[key] {
					continue
				}
				// Keep one path per type and depth, but retain multiplicity:
				// a diamond is ambiguous even when both paths reach one type.
				if previous, exists := nextIndex[key]; exists {
					next[previous].multiple = true
					continue
				}
				nextIndex[key] = len(next)
				path := append(append([]int(nil), item.index...), index)
				next = append(next, selectorFrontier{
					typ: field.Type, index: path, indirect: indirect,
					multiple: item.multiple, key: key,
				})
			}
		}
		if len(candidates) == 1 {
			candidates[0].selection.Receiver = receiver
			return candidates[0], !candidates[0].inaccessible
		}
		if len(candidates) > 1 {
			return selectorCandidate{ambiguous: true}, false
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
			if field.Name == name {
				path := append(append([]int(nil), item.index...), index)
				out = append(out, selectorCandidate{
					selection:    Selection{Kind: SelectionField, Name: name, Receiver: item.typ, DeclaringReceiver: item.typ, Type: field.Type, Index: path},
					fieldType:    field.Type,
					inaccessible: !a.visibleMember(name, a.typeModule(item.typ)),
				})
			}
		}
	}
	methods := a.directMethods(item.typ, methodExpression && !item.indirect)
	genericObjects := make(map[string]ObjectID)
	base := a.resolveAlias(item.typ)
	pointer := a.info.Relations.View(base).Shape() == types.Pointer
	if pointer {
		base, _ = a.info.Relations.View(base).Elem()
	}
	if node, ok := a.typeNode(base); ok && node.Kind == types.Instance {
		base = node.Base
	}
	for _, generic := range a.info.GenericMethods {
		owner := generic.Method.Receiver
		methodPointer := a.info.Relations.View(owner).Shape() == types.Pointer
		if methodPointer {
			owner, _ = a.info.Relations.View(owner).Elem()
		}
		if node, ok := a.typeNode(owner); ok && node.Kind == types.Instance {
			owner = node.Base
		}
		if !a.info.Relations.Identical(a.resolveAlias(base), a.resolveAlias(owner)).OK || methodExpression && !item.indirect && methodPointer && !pointer {
			continue
		}
		methods = append(methods, generic.Method)
		genericObjects[generic.Method.Name] = generic.Object
	}
	for _, method := range methods {
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
			Object:     genericObjects[method.Name],
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
	ref = a.resolveAlias(ref)
	if instance, ok := a.typeNode(ref); ok && instance.Kind == types.Instance {
		fields, _ := a.info.Relations.View(instance.Base).StructFields()
		bindings, ok := a.instanceTypeBindings(instance)
		if !ok {
			return fields
		}
		cache := make(map[types.TypeID]types.TypeRef)
		for i := range fields {
			if resolved, valid := a.substituteType(fields[i].Type, bindings, "fields."+string(instance.ID), cache); valid {
				fields[i].Type = resolved
			}
		}
		return fields
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
	ref = a.resolveAlias(ref)
	if node, ok := a.typeNode(ref); ok && node.Kind == types.Instance {
		ref = a.resolveAlias(node.Base)
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
	return isExported(name) || strings.TrimSpace(modulePath) == strings.TrimSpace(a.info.ModulePath) || modulePath != "" && modulePath == a.definitionModule
}
