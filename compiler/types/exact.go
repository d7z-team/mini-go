package types

// TypeExact reports whether all referenced compiler types have been resolved.
// Only fully resolved graphs are cached. Adding a new node cannot change them;
// Replace and Reindex invalidate the table-owned cache.
func (t *TypeTable) TypeExact(ref TypeRef) bool {
	if !ref.Valid() {
		return false
	}
	if ref.Node == "" {
		return true
	}
	if t == nil {
		return false
	}
	if t.byID == nil || t.indexOwner != t {
		if err := t.index(); err != nil {
			return false
		}
	}
	t.cache.RLock()
	if t.cache.exact[ref] {
		t.cache.RUnlock()
		return true
	}
	seen := map[TypeRef]bool{}
	var exact func(TypeRef) bool
	exact = func(current TypeRef) bool {
		if current.Node == "" || !current.Valid() {
			return true
		}
		if current.Kind == TypeParameter || current.Kind == Instance {
			return false
		}
		if t.cache.exact[current] || seen[current] {
			return true
		}
		seen[current] = true
		node, ok := t.Node(current)
		if !ok {
			return false
		}
		if node.Kind == Named && !node.Alias && !node.Underlying.Valid() {
			return false
		}
		if node.Kind == Array && node.Length == UnknownArrayLength {
			return false
		}
		if !exact(node.AliasTarget) || !exact(node.Underlying) || !exact(node.Elem) || !exact(node.Key) || !exact(node.Constraint) || !exact(node.Base) {
			return false
		}
		for _, item := range node.TypeArgs {
			if !exact(item) {
				return false
			}
		}
		for _, item := range node.Tuple {
			if !exact(item) {
				return false
			}
		}
		for _, field := range node.Fields {
			if !exact(field.Type) {
				return false
			}
		}
		if node.Signature != nil {
			for _, param := range node.Signature.Params {
				if !exact(param.Type) {
					return false
				}
			}
			for _, result := range node.Signature.Results {
				if !exact(result) {
					return false
				}
			}
		}
		for _, method := range node.Methods {
			if !exact(method.Receiver) {
				return false
			}
			for _, param := range method.Signature.Params {
				if !exact(param.Type) {
					return false
				}
			}
			for _, result := range method.Signature.Results {
				if !exact(result) {
					return false
				}
			}
		}
		for _, term := range node.Terms {
			if !exact(term.Type) {
				return false
			}
		}
		return true
	}
	result := exact(ref)
	t.cache.RUnlock()
	// Only a successful traversal proves all visited cycle members exact.
	// Failed or provisional results must never escape into the shared cache.
	if result {
		t.cache.Lock()
		if t.cache.exact == nil {
			t.cache.exact = make(map[TypeRef]bool)
		}
		for current := range seen {
			if len(t.cache.exact) >= len(t.Nodes) {
				break
			}
			t.cache.exact[current] = true
		}
		t.cache.Unlock()
	}
	return result
}
