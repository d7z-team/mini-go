package runtime

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

type runtimeTypeRefMapper func(types.TypeRef) types.TypeRef

func rewriteRuntimeTypeText(text string, mapRef runtimeTypeRefMapper) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	table := &types.TypeTable{}
	parser := types.NewParser("", table)
	ref, err := parser.Parse(text)
	if err != nil {
		return "", false
	}
	if mapRef != nil {
		for i := range table.Nodes {
			rewriteRuntimeTypeNode(&table.Nodes[i], mapRef)
		}
		_ = table.Reindex()
		ref = rewriteRuntimeTypeRef(ref, mapRef)
	}
	return formatRuntimeTypeRef(table, ref, map[types.TypeID]bool{}), true
}

func runtimeSliceElemText(text string) (string, bool) {
	table := &types.TypeTable{}
	parser := types.NewParser("", table)
	ref, err := parser.Parse(strings.TrimSpace(text))
	if err != nil {
		return "", false
	}
	node, ok := table.Node(ref)
	if !ok || node.Kind != types.Slice {
		return "", false
	}
	elem := strings.TrimSpace(formatRuntimeTypeRef(table, node.Elem, map[types.TypeID]bool{}))
	return elem, elem != ""
}

func rewriteRuntimeTypeNode(node *types.TypeNode, mapRef runtimeTypeRefMapper) {
	node.AliasTarget = rewriteRuntimeTypeRef(node.AliasTarget, mapRef)
	node.Underlying = rewriteRuntimeTypeRef(node.Underlying, mapRef)
	node.Elem = rewriteRuntimeTypeRef(node.Elem, mapRef)
	node.Key = rewriteRuntimeTypeRef(node.Key, mapRef)
	for i := range node.Tuple {
		node.Tuple[i] = rewriteRuntimeTypeRef(node.Tuple[i], mapRef)
	}
	for i := range node.Fields {
		node.Fields[i].Type = rewriteRuntimeTypeRef(node.Fields[i].Type, mapRef)
	}
	for i := range node.Methods {
		node.Methods[i].Receiver = rewriteRuntimeTypeRef(node.Methods[i].Receiver, mapRef)
		rewriteRuntimeSignature(&node.Methods[i].Signature, mapRef)
	}
	for i := range node.Terms {
		node.Terms[i].Type = rewriteRuntimeTypeRef(node.Terms[i].Type, mapRef)
	}
	if node.Signature != nil {
		rewriteRuntimeSignature(node.Signature, mapRef)
	}
}

func rewriteRuntimeSignature(signature *types.FunctionSignature, mapRef runtimeTypeRefMapper) {
	if signature == nil {
		return
	}
	for i := range signature.Params {
		signature.Params[i].Type = rewriteRuntimeTypeRef(signature.Params[i].Type, mapRef)
	}
	for i := range signature.Results {
		signature.Results[i] = rewriteRuntimeTypeRef(signature.Results[i], mapRef)
	}
}

func rewriteRuntimeTypeRef(ref types.TypeRef, mapRef runtimeTypeRefMapper) types.TypeRef {
	if mapRef == nil {
		return ref
	}
	return mapRef(ref)
}

func formatRuntimeTypeRef(table *types.TypeTable, ref types.TypeRef, seen map[types.TypeID]bool) string {
	switch ref.Kind {
	case types.Void:
		return "Void"
	case types.Any:
		return "Any"
	case types.Primitive:
		return types.PrimitiveName(ref.Primitive)
	case types.Named:
		if ref.Named.ModulePath == "" {
			return string(ref.Named.DeclID)
		}
		return ref.Named.ModulePath + "." + string(ref.Named.DeclID)
	}
	if table == nil || ref.Node == "" || seen[ref.Node] {
		return types.KindName(ref.Kind)
	}
	node, ok := table.Node(ref)
	if !ok {
		return types.KindName(ref.Kind)
	}
	seen[ref.Node] = true
	defer delete(seen, ref.Node)
	switch node.Kind {
	case types.Slice:
		return "Slice<" + formatRuntimeTypeRef(table, node.Elem, seen) + ">"
	case types.Array:
		return fmt.Sprintf("Array<%d, %s>", node.Length, formatRuntimeTypeRef(table, node.Elem, seen))
	case types.Map:
		return "Map<" + formatRuntimeTypeRef(table, node.Key, seen) + ", " + formatRuntimeTypeRef(table, node.Elem, seen) + ">"
	case types.Pointer:
		return "Ptr<" + formatRuntimeTypeRef(table, node.Elem, seen) + ">"
	case types.Waitable:
		prefix := "Waitable<"
		switch node.Direction {
		case types.ChannelReceive:
			prefix = "ReceiveWaitable<"
		case types.ChannelSend:
			prefix = "SendWaitable<"
		}
		return prefix + formatRuntimeTypeRef(table, node.Elem, seen) + ">"
	case types.Function:
		if node.Signature == nil {
			return "function() Void"
		}
		return formatRuntimeFunctionSignature(table, *node.Signature, seen)
	case types.Tuple:
		items := make([]string, len(node.Tuple))
		for i, item := range node.Tuple {
			items[i] = formatRuntimeTypeRef(table, item, seen)
		}
		return "tuple(" + strings.Join(items, ", ") + ")"
	case types.Struct:
		fields := make([]string, len(node.Fields))
		for i, field := range node.Fields {
			fields[i] = formatCanonicalStructField(field.Name, formatRuntimeTypeRef(table, field.Type, seen), field.Tag, field.Embedded)
		}
		return "struct{" + strings.Join(fields, ",") + "}"
	case types.Interface:
		members := make([]string, 0, len(node.Terms)+len(node.Methods))
		union := make([]string, 0)
		for _, term := range node.Terms {
			member := formatRuntimeTypeRef(table, term.Type, seen)
			if term.Approx {
				member = "~" + member
			}
			if term.Union {
				union = append(union, member)
			} else {
				members = append(members, member)
			}
		}
		if len(union) != 0 {
			members = append([]string{strings.Join(union, "|")}, members...)
		}
		for _, method := range node.Methods {
			members = append(members, method.Name+":"+formatRuntimeFunctionSignature(table, method.Signature, seen))
		}
		return "interface{" + strings.Join(members, ",") + "}"
	default:
		if node.Identity.ModulePath != "" || node.Identity.DeclID != "" {
			if node.Identity.ModulePath == "" {
				return string(node.Identity.DeclID)
			}
			return node.Identity.ModulePath + "." + string(node.Identity.DeclID)
		}
		return types.KindName(node.Kind)
	}
}

func formatRuntimeFunctionSignature(table *types.TypeTable, signature types.FunctionSignature, seen map[types.TypeID]bool) string {
	params := make([]string, len(signature.Params))
	for i, param := range signature.Params {
		params[i] = formatRuntimeTypeRef(table, param.Type, seen)
		if signature.Variadic && i == len(params)-1 {
			params[i] = canonicalVariadicFunctionParam + params[i]
		}
	}
	results := make([]string, len(signature.Results))
	for i, result := range signature.Results {
		results[i] = formatRuntimeTypeRef(table, result, seen)
	}
	result := " Void"
	if len(results) == 1 {
		result = " " + results[0]
	} else if len(results) > 1 {
		result = " tuple(" + strings.Join(results, ", ") + ")"
	}
	return "function(" + strings.Join(params, ", ") + ")" + result
}

func runtimeTypeElem(t vmType, kind types.Kind) (vmType, bool) {
	underlying := t.Underlying()
	if underlying.Ref.Kind != kind {
		return vmType{}, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != kind || !node.Elem.Valid() {
		return vmType{}, false
	}
	return underlying.derived(node.Elem), true
}

func (t vmType) Underlying() vmType {
	if t.Table == nil || t.Ref.Kind != types.Named {
		return t
	}
	if t.underlyingRef != nil {
		return t.derived(*t.underlyingRef)
	}
	return t.derived(t.Table.Underlying(t.Ref))
}

func (t vmType) Node() (types.TypeNode, bool) {
	if t.Table == nil {
		return types.TypeNode{}, false
	}
	return t.Table.Node(t.Ref)
}

func namedRuntimeTypeRef(modulePath, declID string) vmType {
	text := declID
	if modulePath != "" {
		text = modulePath + "." + declID
	}
	return vmType{Ref: types.TypeRef{Kind: types.Named, Named: types.TypeKey{
		ModulePath: modulePath,
		DeclID:     types.DeclID(declID),
	}}, text: text}
}

func canonicalRuntimeTypeRef(ref types.TypeRef) types.TypeRef {
	if ref.Kind == types.Named && ref.Named.ModulePath != "" && ref.Named.DeclID != "" {
		ref.Node = ""
	}
	return ref
}
