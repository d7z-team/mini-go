package types

import (
	"errors"
	"sort"

	"github.com/d7z-team/mini-go/compiler/token"
)

func (t *TypeTable) Underlying(ref TypeRef) TypeRef {
	if t == nil || ref.Kind != Named {
		return ref
	}
	if t.indexOwner != t {
		if err := t.index(); err != nil {
			return ref
		}
	}
	t.cache.RLock()
	underlying, ok := t.cache.underlying[ref]
	t.cache.RUnlock()
	if ok {
		return underlying
	}
	original := ref
	// A valid chain cannot visit more named nodes than the table contains.
	// Bounding the walk avoids allocating a cycle-detection map on every
	// runtime type query while still terminating malformed cyclic tables.
	for steps := 0; ref.Kind == Named && steps <= len(t.Nodes); steps++ {
		node, ok := t.nodeForRef(ref)
		if !ok {
			t.cache.Lock()
			t.cache.underlying[original] = ref
			t.cache.Unlock()
			return ref
		}
		if node.Alias {
			if !node.AliasTarget.Valid() {
				t.cache.Lock()
				t.cache.underlying[original] = ref
				t.cache.Unlock()
				return ref
			}
			ref = node.AliasTarget
			continue
		}
		if node.Underlying.Valid() {
			ref = node.Underlying
			continue
		}
		break
	}
	t.cache.Lock()
	t.cache.underlying[original] = ref
	t.cache.Unlock()
	return ref
}

// ReachableTable returns the deterministic type-node closure required by roots.
func (t *TypeTable) ReachableTable(roots ...TypeRef) (*TypeTable, error) {
	if t == nil {
		return nil, errors.New("nil type table")
	}
	seen := make(map[TypeID]bool)
	queue := append([]TypeRef(nil), roots...)
	for len(queue) != 0 {
		ref := queue[0]
		queue = queue[1:]
		node, ok := t.nodeForRef(ref)
		if !ok || seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		queue = append(queue, nodeRefs(node)...)
	}
	nodes := make([]TypeNode, 0, len(seen))
	for _, node := range t.Nodes {
		if seen[node.ID] {
			nodes = append(nodes, node)
		}
	}
	out := NewTable(nodes...)
	if out == nil {
		return nil, errors.New("reachable type table is invalid")
	}
	return out, nil
}

func (t *TypeTable) IsNamed(ref TypeRef) bool {
	return ref.Kind == Named
}

func (t *TypeTable) IsInterface(ref TypeRef) (TypeNode, bool) {
	underlying := t.Underlying(ref)
	if ref.Kind == Named && underlying.Kind == Any {
		return TypeNode{Kind: Interface}, true
	}
	if underlying.Kind != Interface {
		return TypeNode{}, false
	}
	shape, ok := t.nodeForRef(underlying)
	if !ok {
		return TypeNode{}, false
	}
	if ref.Kind == Named {
		if named, exists := t.nodeForRef(ref); exists && len(named.Methods) != 0 {
			shape.Methods = append([]Method(nil), named.Methods...)
		}
	}
	return shape, true
}

func (t *TypeTable) IsFunction(ref TypeRef) (FunctionSignature, bool) {
	underlying := t.Underlying(ref)
	if underlying.Kind != Function || underlying.Node == "" {
		return FunctionSignature{}, false
	}
	node, ok := t.Node(underlying)
	if !ok || node.Signature == nil {
		return FunctionSignature{}, false
	}
	return *node.Signature, true
}

func (t *TypeTable) Assignable(source, target TypeRef) bool {
	return NewRelations(t).Assignable(source, target).OK
}

func (t *TypeTable) Implements(source, target TypeRef) bool {
	return NewRelations(t).Implements(source, target).OK
}

type waitableInfo struct {
	Direction ChannelDir
	Elem      TypeRef
}

func methodKey(method Method) string {
	if method.ModulePath == "" || method.Name == "" || token.IsExportedName(method.Name) {
		return method.Name
	}
	return method.ModulePath + ":" + method.Name
}

func SortedNodeIDs(t *TypeTable) []TypeID {
	if t == nil {
		return nil
	}
	ids := make([]TypeID, 0, len(t.Nodes))
	for _, node := range t.Nodes {
		ids = append(ids, node.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
