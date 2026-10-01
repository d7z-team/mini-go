package types

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

func NewTable(nodes ...TypeNode) *TypeTable {
	table := &TypeTable{Nodes: append([]TypeNode(nil), nodes...)}
	if err := table.index(); err != nil {
		return nil
	}
	return table
}

func (t *TypeTable) index() error {
	if t == nil {
		return errors.New("nil type table")
	}
	t.byID = make(map[TypeID]int, len(t.Nodes))
	t.byName = make(map[TypeKey]TypeID)
	t.cache = &typeTableCache{
		underlying: make(map[TypeRef]TypeRef),
		formatted:  make(map[TypeRef]string),
	}
	for i := range t.Nodes {
		node := &t.Nodes[i]
		if node.ID == "" {
			return fmt.Errorf("type node %d has no id", i)
		}
		if _, exists := t.byID[node.ID]; exists {
			return fmt.Errorf("duplicate type node %q", node.ID)
		}
		t.byID[node.ID] = i
		if !emptyTypeKey(node.Identity) {
			identity := node.Identity
			if _, exists := t.byName[identity]; exists {
				return fmt.Errorf("duplicate type identity %s.%s", node.Identity.ModulePath, node.Identity.DeclID)
			}
			t.byName[identity] = node.ID
		}
	}
	t.indexOwner = t
	return nil
}

func (t *TypeTable) Reindex() error { return t.index() }

func (t *TypeTable) Node(ref TypeRef) (TypeNode, bool) {
	if t == nil || ref.Node == "" {
		return TypeNode{}, false
	}
	if t.byID == nil || t.indexOwner != t {
		if err := t.index(); err != nil {
			return TypeNode{}, false
		}
	}
	i, ok := t.byID[ref.Node]
	if !ok {
		return TypeNode{}, false
	}
	return t.Nodes[i], true
}

func (t *TypeTable) nodeForRef(ref TypeRef) (TypeNode, bool) {
	if ref.Kind == Named && ref.Node == "" {
		return t.Named(ref.Named)
	}
	return t.Node(ref)
}

func (t *TypeTable) Named(key TypeKey) (TypeNode, bool) {
	if t == nil {
		return TypeNode{}, false
	}
	if t.byName == nil || t.indexOwner != t {
		if err := t.index(); err != nil {
			return TypeNode{}, false
		}
	}
	id, ok := t.byName[key]
	if !ok {
		return TypeNode{}, false
	}
	node, ok := t.Node(TypeRef{Kind: Named, Node: id})
	return node, ok
}

func (t *TypeTable) Add(node TypeNode) error {
	if t == nil {
		return errors.New("nil type table")
	}
	if node.ID == "" {
		return errors.New("type node has no id")
	}
	if t.byID == nil || t.indexOwner != t {
		if err := t.index(); err != nil {
			return err
		}
	}
	if _, exists := t.byID[node.ID]; exists {
		return fmt.Errorf("duplicate type node %q", node.ID)
	}
	identity := TypeKey{}
	if !emptyTypeKey(node.Identity) {
		identity = node.Identity
		if _, exists := t.byName[identity]; exists {
			return fmt.Errorf("duplicate type identity %s.%s", node.Identity.ModulePath, node.Identity.DeclID)
		}
	}
	index := len(t.Nodes)
	t.Nodes = append(t.Nodes, node)
	if t.cache == nil {
		t.cache = &typeTableCache{underlying: make(map[TypeRef]TypeRef), formatted: make(map[TypeRef]string)}
	} else {
		t.cache.Lock()
		clear(t.cache.underlying)
		clear(t.cache.formatted)
		t.cache.Unlock()
	}
	t.byID[node.ID] = index
	if !emptyTypeKey(identity) {
		t.byName[identity] = node.ID
	}
	return nil
}

// Replace updates an existing compiler type node without changing its stable
// identity. It is used to seal forward-declared named and type-parameter nodes
// after their underlying type or constraint has been resolved.
func (t *TypeTable) Replace(node TypeNode) error {
	if t == nil {
		return errors.New("nil type table")
	}
	if node.ID == "" {
		return errors.New("type node has no id")
	}
	if t.byID == nil || t.indexOwner != t {
		if err := t.index(); err != nil {
			return err
		}
	}
	index, ok := t.byID[node.ID]
	if !ok {
		return fmt.Errorf("unknown type node %q", node.ID)
	}
	previous := t.Nodes[index]
	if previous.Identity != node.Identity {
		return fmt.Errorf("type node %q identity changed", node.ID)
	}
	t.Nodes[index] = node
	if t.cache == nil {
		t.cache = &typeTableCache{underlying: make(map[TypeRef]TypeRef), formatted: make(map[TypeRef]string)}
	} else {
		t.cache.Lock()
		clear(t.cache.underlying)
		clear(t.cache.formatted)
		clear(t.cache.exact)
		t.cache.Unlock()
	}
	return nil
}

// DeclaredNamed returns all named type declarations owned by modulePath in
// declaration-name order. The returned nodes are copies of table entries.
func (t *TypeTable) DeclaredNamed(modulePath string) []TypeNode {
	if t == nil {
		return nil
	}
	modulePath = strings.TrimSpace(modulePath)
	out := make([]TypeNode, 0)
	for _, node := range t.Nodes {
		if node.Kind == Named && node.Identity.ModulePath == modulePath && node.Identity.DeclID != "" {
			out = append(out, node)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.DeclID < out[j].Identity.DeclID })
	return out
}

// DefinedNamed returns the non-alias named types owned by modulePath in
// declaration-name order. The returned nodes are copies of table entries.
func (t *TypeTable) DefinedNamed(modulePath string) []TypeNode {
	declared := t.DeclaredNamed(modulePath)
	out := declared[:0]
	for _, node := range declared {
		if !node.Alias {
			out = append(out, node)
		}
	}
	return out
}

// Ref returns the canonical reference for a table node.
func Ref(node TypeNode) TypeRef {
	return TypeRef{Kind: node.Kind, Named: node.Identity, Node: node.ID}
}
