package runtime

import (
	"strconv"

	"github.com/d7z-team/mini-go/compiler/types"
)

// ownedStorageType retains only the type graph needed to interpret a live
// value. External named types are resolved before cloning, and table-local
// node IDs are rebased so a data owner never needs to retain a code revision.
func ownedStorageType(root vmType, resolve func(any) vmType) (vmType, error) {
	if root.Ref.Kind == types.Primitive || root.Ref.Kind == types.Any || root.Ref.Kind == types.Void {
		root.Table = nil
		return root, nil
	}
	if root.Table == nil {
		return root, nil
	}
	type sourceNode struct {
		table *types.TypeTable
		id    types.TypeID
	}
	owned := &types.TypeTable{}
	seen := make(map[sourceNode]types.TypeRef)
	named := make(map[types.TypeKey]types.TypeRef)
	shapes := make(map[string]types.TypeRef)
	var copyRef func(vmType) types.TypeRef
	copySignature := func(signature types.FunctionSignature, table *types.TypeTable) types.FunctionSignature {
		params := make([]types.TypeParam, len(signature.Params))
		for index, param := range signature.Params {
			params[index].Type = copyRef(runtimeTypeWithTable(param.Type, table))
		}
		results := make([]types.TypeRef, len(signature.Results))
		for index, result := range signature.Results {
			results[index] = copyRef(runtimeTypeWithTable(result, table))
		}
		return types.FunctionSignature{Params: params, Results: results, Variadic: signature.Variadic}
	}
	copyRef = func(typ vmType) types.TypeRef {
		ref, table := typ.Ref, typ.Table
		if !ref.Valid() || ref.Kind == types.Primitive || ref.Kind == types.Any || ref.Kind == types.Void {
			return ref
		}
		var node types.TypeNode
		var found bool
		if ref.Kind == types.Named {
			if previous, ok := named[ref.Named]; ok && ref.Named != (types.TypeKey{}) {
				return previous
			}
			node, found = table.Node(ref)
			if !found {
				node, found = table.Named(ref.Named)
			}
			if (!found || (!node.Underlying.Valid() && !node.AliasTarget.Valid())) && resolve != nil {
				resolved := resolve(typ)
				if resolved.Ref.Kind != types.Named {
					return copyRef(resolved)
				}
				ref, table = resolved.Ref, resolved.Table
				node, found = table.Node(ref)
				if !found {
					node, found = table.Named(ref.Named)
				}
			}
		} else {
			if previous, ok := shapes[typ.String()]; ok {
				return previous
			}
			node, found = table.Node(ref)
		}
		if !found {
			// An external named identity can occur in a signature without a
			// body in the current program. It owns no source table or payload.
			if ref.Kind == types.Named {
				ref.Node = ""
			}
			return ref
		}
		key := sourceNode{table: table, id: node.ID}
		if previous, ok := seen[key]; ok {
			return previous
		}
		index := len(owned.Nodes)
		node.ID = types.TypeID("storage/" + strconv.Itoa(index))
		copiedRef := types.Ref(node)
		seen[key] = copiedRef
		if node.Kind == types.Named && node.Identity != (types.TypeKey{}) {
			named[node.Identity] = copiedRef
		} else {
			shapes[typ.String()] = copiedRef
		}
		// Install the identity before traversing self-referential pointers,
		// method receivers and mutually recursive named declarations.
		owned.Nodes = append(owned.Nodes, types.TypeNode{})
		for _, child := range []*types.TypeRef{&node.AliasTarget, &node.Underlying, &node.Elem, &node.Key, &node.Constraint, &node.Base} {
			*child = copyRef(runtimeTypeWithTable(*child, table))
		}
		if node.Signature != nil {
			signature := copySignature(*node.Signature, table)
			node.Signature = &signature
		}
		node.Fields = append([]types.Field(nil), node.Fields...)
		for index := range node.Fields {
			node.Fields[index].Type = copyRef(runtimeTypeWithTable(node.Fields[index].Type, table))
		}
		node.Methods = append([]types.Method(nil), node.Methods...)
		for index := range node.Methods {
			method := &node.Methods[index]
			method.Receiver = copyRef(runtimeTypeWithTable(method.Receiver, table))
			method.Signature = copySignature(method.Signature, table)
		}
		node.Terms = append([]types.TypeTerm(nil), node.Terms...)
		for index := range node.Terms {
			node.Terms[index].Type = copyRef(runtimeTypeWithTable(node.Terms[index].Type, table))
		}
		node.Tuple = append([]types.TypeRef(nil), node.Tuple...)
		for index := range node.Tuple {
			node.Tuple[index] = copyRef(runtimeTypeWithTable(node.Tuple[index], table))
		}
		node.TypeArgs = append([]types.TypeRef(nil), node.TypeArgs...)
		for index := range node.TypeArgs {
			node.TypeArgs[index] = copyRef(runtimeTypeWithTable(node.TypeArgs[index], table))
		}
		owned.Nodes[index] = node
		return copiedRef
	}
	ref := copyRef(root)
	if err := owned.Reindex(); err != nil {
		return vmType{}, err
	}
	result := runtimeTypeWithTable(ref, owned)
	result.text, result.standalone = root.text, root.standalone
	return result, nil
}
