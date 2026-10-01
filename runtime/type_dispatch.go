package runtime

import "github.com/d7z-team/mini-go/compiler/types"

type preparedTypeSwitch struct {
	subject, fallback, fallbackLocal int
	cases                            []preparedTypeCase
	concrete                         map[typeDispatchIdentity]int
	concreteEnd                      int
}

type typeDispatchIdentity struct {
	ref     types.TypeRef
	pointer bool
}

// Only identities whose equality is independent of a module's structural
// graph enter the table. Interfaces and structural shapes retain ordered
// matching; the table never accumulates runtime reflection types.
func typeDispatchIdentityOf(typ vmType) (typeDispatchIdentity, bool) {
	if !typ.Valid() {
		return typeDispatchIdentity{}, true
	}
	if typ.Table != nil {
		typ = runtimeTypeWithTable(types.NewRelations(typ.Table).ResolveAlias(typ.Ref), typ.Table)
	}
	key := typeDispatchIdentity{}
	if typ.Ref.Kind == types.Pointer {
		elem, ok := typ.PointerElem()
		if !ok {
			return key, false
		}
		typ, key.pointer = elem, true
		if typ.Table != nil {
			typ = runtimeTypeWithTable(types.NewRelations(typ.Table).ResolveAlias(typ.Ref), typ.Table)
		}
	}
	switch typ.Ref.Kind {
	case types.Primitive:
		if typ.Ref.Primitive < types.PrimitiveBool || typ.Ref.Primitive > types.PrimitiveComplex128 {
			return key, false
		}
		key.ref = types.Builtin(typ.Ref.Primitive)
	case types.Named:
		if !key.pointer {
			if _, isInterface := typ.InterfaceInfo(); isInterface || typ.ShapeKind() == types.Function {
				return key, false
			}
		}
		key.ref = types.TypeRef{Kind: types.Named, Named: typ.Ref.Named}
	default:
		return key, false
	}
	return key, true
}

type preparedTypeCase struct {
	typ             vmType
	target, binding int
	original        bool
}
