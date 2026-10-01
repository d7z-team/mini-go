package types

import "testing"

func TestTypeExactTracksForwardResolutionAndTableChanges(t *testing.T) {
	named := TypeNode{ID: "T", Kind: Named, Identity: TypeKey{ModulePath: "sample", DeclID: "T"}}
	array := TypeNode{ID: "array", Kind: Array, Elem: Builtin(PrimitiveInt), Length: UnknownArrayLength}
	table := NewTable(named, array)
	ref := Ref(named)
	if table.TypeExact(ref) || table.TypeExact(Ref(array)) {
		t.Fatal("unresolved declarations are exact")
	}
	named.Underlying = Ref(array)
	if err := table.Replace(named); err != nil {
		t.Fatal(err)
	}
	if table.TypeExact(ref) {
		t.Fatal("unresolved nested array is exact")
	}
	array.Length = 2
	if err := table.Replace(array); err != nil {
		t.Fatal(err)
	}
	if !table.TypeExact(ref) {
		t.Fatal("resolved array remained inexact")
	}
	copied := CloneTable(*table)
	array.Length = UnknownArrayLength
	if err := table.Replace(array); err != nil {
		t.Fatal(err)
	}
	if table.TypeExact(ref) || !copied.TypeExact(ref) {
		t.Fatal("cache leaked across table owners")
	}
	array.Length = 3
	table.Nodes[1] = array
	if err := table.Reindex(); err != nil {
		t.Fatal(err)
	}
	if !table.TypeExact(ref) {
		t.Fatal("Reindex retained obsolete exactness")
	}
}

func TestTypeExactDoesNotCacheProvisionalCycleResults(t *testing.T) {
	a := TypeNode{ID: "a", Kind: Struct}
	b := TypeNode{ID: "b", Kind: Pointer, Elem: Ref(a)}
	missing := TypeRef{Kind: Array, Node: "missing"}
	a.Fields = []Field{{Name: "Cycle", Type: Ref(b)}, {Name: "Later", Type: missing}}
	table := NewTable(a, b)
	if table.TypeExact(Ref(a)) || table.TypeExact(Ref(b)) {
		t.Fatal("cycle hid a missing peer")
	}
	if err := table.Add(TypeNode{ID: "missing", Kind: Array, Length: 2, Elem: Builtin(PrimitiveInt)}); err != nil {
		t.Fatal(err)
	}
	if !table.TypeExact(Ref(a)) || !table.TypeExact(Ref(b)) {
		t.Fatal("Add did not invalidate inexact results")
	}
	parameter := TypeNode{ID: "P", Kind: TypeParameter, Constraint: AnyType()}
	if err := table.Add(parameter); err != nil {
		t.Fatal(err)
	}
	if table.TypeExact(Ref(parameter)) {
		t.Fatal("type parameter is not concrete")
	}
	var absent *TypeTable
	if absent.TypeExact(TypeRef{}) || !absent.TypeExact(Builtin(PrimitiveInt)) || absent.TypeExact(Ref(a)) {
		t.Fatal("invalid, primitive or missing-table boundary")
	}
}
