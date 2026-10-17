package types

import (
	"errors"
	"fmt"
)

func (t *TypeTable) Validate() error {
	return t.validate(false)
}

// ValidateCompiler accepts compiler-only type parameters and instances. Runtime
// artifacts must continue to call Validate, which rejects both kinds.
func (t *TypeTable) ValidateCompiler() error {
	return t.validate(true)
}

func (t *TypeTable) validate(compiler bool) error {
	if err := t.rebuildIndexes(); err != nil {
		return err
	}
	for _, node := range t.Nodes {
		if !validNodeShape(node, compiler) {
			return fmt.Errorf("type node %q: invalid %s node shape", node.ID, KindName(node.Kind))
		}
		for _, ref := range nodeRefs(node) {
			if ref.Kind == Invalid {
				continue
			}
			if !ref.Valid() {
				return fmt.Errorf("type node %q: invalid type reference %s", node.ID, Format(ref))
			}
			if ref.Node != "" {
				if _, ok := t.Node(ref); !ok {
					return fmt.Errorf("type node %q: unknown child node %q", node.ID, ref.Node)
				}
			}
		}
	}
	if err := t.validateAliasChains(); err != nil {
		return err
	}
	for _, node := range t.Nodes {
		if node.Kind != Function || node.Signature == nil || !node.Signature.Variadic {
			continue
		}
		if len(node.Signature.Params) == 0 {
			return fmt.Errorf("type node %q: %w", node.ID, errors.New("invalid function type: variadic signature has no parameters"))
		}
		last := node.Signature.Params[len(node.Signature.Params)-1].Type
		if last.Kind != Slice && t.Underlying(last).Kind != Slice {
			return fmt.Errorf("type node %q: %w", node.ID, errors.New("invalid function type: variadic parameter must be a slice"))
		}
	}
	return nil
}

func (t *TypeTable) validateAliasChains() error {
	for _, root := range t.Nodes {
		if root.Kind != Named || !root.Alias {
			continue
		}
		seen := make(map[TypeID]struct{})
		ref := Ref(root)
		for ref.Kind == Named {
			node, ok := t.nodeForRef(ref)
			if !ok || !node.Alias {
				break
			}
			if _, exists := seen[node.ID]; exists {
				return fmt.Errorf("type node %q: alias cycle", root.ID)
			}
			seen[node.ID] = struct{}{}
			ref = node.AliasTarget
		}
	}
	return nil
}

func validNodeShape(node TypeNode, compiler bool) bool {
	switch node.Kind {
	case Primitive:
		return node.Primitive != PrimitiveInvalid
	case Named:
		return node.Identity.ModulePath != "" && node.Identity.DeclID != "" && (node.Alias || node.Underlying.Valid())
	case Slice, Pointer:
		return node.Elem.Valid()
	case Array:
		return node.Elem.Valid() && (node.Length >= 0 || compiler && node.Length == UnknownArrayLength)
	case Map:
		return node.Key.Valid() && node.Elem.Valid()
	case Waitable:
		return node.Elem.Valid() && node.Direction >= ChannelBoth && node.Direction <= ChannelSend
	case Function:
		return node.Signature != nil && (!node.Signature.Variadic || len(node.Signature.Params) > 0)
	case Tuple:
		return true
	case Struct, Interface:
		return true
	case TypeParameter:
		return compiler && node.Constraint.Valid()
	case Instance:
		return compiler && node.Base.Valid() && len(node.TypeArgs) != 0
	default:
		return false
	}
}

func nodeRefs(node TypeNode) []TypeRef {
	refs := make([]TypeRef, 0, 6+len(node.Fields)+len(node.Tuple)+len(node.Terms)+len(node.TypeArgs))
	refs = append(refs, node.AliasTarget, node.Underlying, node.Elem, node.Key, node.Constraint, node.Base)
	refs = append(refs, node.TypeArgs...)
	refs = append(refs, node.Tuple...)
	if node.Signature != nil {
		for _, param := range node.Signature.Params {
			refs = append(refs, param.Type)
		}
		refs = append(refs, node.Signature.Results...)
	}
	for _, field := range node.Fields {
		refs = append(refs, field.Type)
	}
	for _, method := range node.Methods {
		refs = append(refs, method.Receiver)
		for _, param := range method.Signature.Params {
			refs = append(refs, param.Type)
		}
		refs = append(refs, method.Signature.Results...)
	}
	for _, term := range node.Terms {
		refs = append(refs, term.Type)
	}
	return refs
}
