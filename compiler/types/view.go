package types

// TypeView is the structured read API for semantic type predicates.
// It keeps canonical type text at the bytecode boundary and gives compiler/runtime
// code a single place to ask about Go type shapes and relations.
type TypeView struct {
	Table *TypeTable
	Ref   TypeRef
}

func View(table *TypeTable, ref TypeRef) TypeView {
	return TypeView{Table: table, Ref: ref}
}

func (v TypeView) Valid() bool {
	return v.Ref.Valid()
}

func (v TypeView) Underlying() TypeRef {
	if v.Table == nil {
		return v.Ref
	}
	return v.Table.Underlying(v.Ref)
}

func (v TypeView) Shape() Kind {
	if !v.Ref.Valid() {
		return Invalid
	}
	underlying := v.Underlying()
	if v.Ref.Kind == Named && underlying.Kind == Any {
		return Interface
	}
	return underlying.Kind
}

func (v TypeView) IsNamed() bool {
	return v.Ref.Kind == Named
}

func (v TypeView) IsAlias() bool {
	node, ok := v.namedNode()
	return ok && node.Alias
}

func (v TypeView) Primitive() (PrimitiveKind, bool) {
	ref := v.Underlying()
	if ref.Kind != Primitive {
		return PrimitiveInvalid, false
	}
	return ref.Primitive, true
}

func (v TypeView) NumericInfo() (NumericInfo, bool) {
	return NumericTypeInfo(v.Table, v.Ref)
}

// Constraint returns the source constraint of a type parameter.
func (v TypeView) Constraint() (TypeRef, bool) {
	if v.Table == nil || v.Ref.Kind != TypeParameter {
		return TypeRef{}, false
	}
	node, ok := v.Table.nodeForRef(v.Ref)
	return node.Constraint, ok && node.Kind == TypeParameter && node.Constraint.Valid()
}

// TypeSetTerms returns the explicit terms of an interface type set.
func (v TypeView) TypeSetTerms() ([]TypeTerm, bool) {
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Interface || !node.TypeSet || len(node.Terms) == 0 {
		return nil, false
	}
	return append([]TypeTerm(nil), node.Terms...), true
}

func (v TypeView) ComparableConstraint() bool {
	node, ok := v.underlyingNode()
	return ok && node.Kind == Interface && node.TypeSet && node.Name == "comparable"
}

func (v TypeView) Elem() (TypeRef, bool) {
	node, ok := v.underlyingNode()
	if !ok {
		return TypeRef{}, false
	}
	switch node.Kind {
	case Slice, Pointer, Waitable:
		return node.Elem, node.Elem.Valid()
	default:
		return TypeRef{}, false
	}
}

func (v TypeView) Array() (int64, TypeRef, bool) {
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Array || !node.Elem.Valid() {
		return 0, TypeRef{}, false
	}
	return node.Length, node.Elem, true
}

func (v TypeView) Map() (TypeRef, TypeRef, bool) {
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Map || !node.Key.Valid() || !node.Elem.Valid() {
		return TypeRef{}, TypeRef{}, false
	}
	return node.Key, node.Elem, true
}

func (v TypeView) Waitable() (ChannelDir, TypeRef, bool) {
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Waitable || !node.Elem.Valid() {
		return ChannelInvalid, TypeRef{}, false
	}
	return node.Direction, node.Elem, true
}

func (v TypeView) Function() (FunctionSignature, bool) {
	if primitive, ok := v.Primitive(); ok && primitive == PrimitiveFunction {
		return FunctionSignature{}, true
	}
	if v.Table != nil {
		return v.Table.IsFunction(v.Ref)
	}
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Function || node.Signature == nil {
		return FunctionSignature{}, false
	}
	return *node.Signature, true
}

func (v TypeView) StructFields() ([]Field, bool) {
	if v.Table != nil {
		if node, ok := v.Table.nodeForRef(v.Ref); ok && node.Kind == Named && len(node.Fields) != 0 {
			return append([]Field(nil), node.Fields...), true
		}
	}
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Struct {
		return nil, false
	}
	return append([]Field(nil), node.Fields...), true
}

func (v TypeView) Interface() ([]Method, []TypeTerm, bool, bool) {
	if v.Ref.Kind == Named && v.Underlying().Kind == Any {
		return []Method{}, []TypeTerm{}, false, true
	}
	if v.Table != nil {
		node, ok := v.Table.IsInterface(v.Ref)
		if !ok {
			return nil, nil, false, false
		}
		return append([]Method(nil), node.Methods...), append([]TypeTerm(nil), node.Terms...), node.TypeSet, true
	}
	node, ok := v.underlyingNode()
	if !ok || node.Kind != Interface {
		return nil, nil, false, false
	}
	methods := append([]Method(nil), node.Methods...)
	terms := append([]TypeTerm(nil), node.Terms...)
	return methods, terms, node.TypeSet, true
}

func (v TypeView) Nilable() bool {
	if !v.Ref.Valid() {
		return false
	}
	if v.Ref.Kind == Any {
		return true
	}
	if primitive, ok := v.Primitive(); ok {
		switch primitive {
		case PrimitiveFunction:
			return true
		default:
			return false
		}
	}
	switch v.Shape() {
	case Slice, Map, Pointer, Waitable, Function, Interface:
		return true
	default:
		return false
	}
}

func (v TypeView) Comparable() bool {
	return v.comparable(map[TypeID]bool{}, false)
}

func (v TypeView) StrictlyComparable() bool {
	return v.comparable(map[TypeID]bool{}, true)
}

func (v TypeView) Ordered() bool {
	if primitive, ok := v.Primitive(); ok {
		if primitive == PrimitiveString {
			return true
		}
		info, _, ok := primitiveNumericInfo(primitive)
		return ok && info != NumericComplex
	}
	return false
}

func (v TypeView) comparable(seen map[TypeID]bool, strict bool) bool {
	if !v.Ref.Valid() {
		return false
	}
	if v.Ref.Kind == Any {
		return !strict
	}
	if primitive, ok := v.Primitive(); ok {
		if primitive == PrimitiveBool || primitive == PrimitiveString {
			return true
		}
		_, _, ok := primitiveNumericInfo(primitive)
		return ok
	}
	ref := v.Underlying()
	if ref.Node != "" {
		if seen[ref.Node] {
			return true
		}
		seen[ref.Node] = true
		defer delete(seen, ref.Node)
	}
	node, ok := v.underlyingNode()
	if !ok {
		return false
	}
	switch node.Kind {
	case Pointer, Waitable:
		return true
	case Interface:
		return !strict
	case Array:
		return View(v.Table, node.Elem).comparable(seen, strict)
	case Struct:
		for _, field := range node.Fields {
			if !View(v.Table, field.Type).comparable(seen, strict) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (v TypeView) namedNode() (TypeNode, bool) {
	if v.Table == nil || v.Ref.Kind != Named {
		return TypeNode{}, false
	}
	return v.Table.nodeForRef(v.Ref)
}

func (v TypeView) underlyingNode() (TypeNode, bool) {
	if v.Table == nil {
		return TypeNode{}, false
	}
	return v.Table.nodeForRef(v.Underlying())
}
