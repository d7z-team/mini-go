package runtime

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPreparedLayoutTracksNestedReferenceAndDynamicLanes(t *testing.T) {
	table := &types.TypeTable{}
	parser := types.NewParser("layout/test", table)
	ref, err := parser.Parse("struct{Bytes:Slice<Int32>,Value:Any}")
	if err != nil {
		t.Fatal(err)
	}
	typ := runtimeTypeWithTable(ref, table)
	registry := newPreparedLayoutRegistry()
	layout, err := registry.layoutFor(typ)
	if err != nil {
		t.Fatal(err)
	}
	if layout.ID == 0 || layout.Stride < 56 {
		t.Fatalf("prepared layout = %#v", layout)
	}
	if len(layout.References) != 1 || len(layout.Dynamics) != 1 {
		t.Fatalf("prepared references = %#v dynamics=%#v", layout.References, layout.Dynamics)
	}
	if got, ok := registry.byID(layout.ID); !ok || got.Stride != layout.Stride {
		t.Fatalf("layout registry lookup = %#v, %v", got, ok)
	}
}

func TestPreparedLayoutCarriesAggregateChildrenIntoExecutionPlan(t *testing.T) {
	registry := newPreparedLayoutRegistry()
	typ := runtimeTypeFromText("struct{Items:Array<2,struct{Value:Int64}>,Values:Slice<Int64>}")
	layout, err := registry.layoutFor(typ)
	if err != nil {
		t.Fatal(err)
	}
	if layout.StructPlan == nil || len(layout.StructPlan.fields) != 2 {
		t.Fatalf("struct execution plan = %#v", layout.StructPlan)
	}
	items := layout.StructPlan.fields[0].layout
	if items.ArrayElement == nil || items.ArrayElement.StructPlan == nil {
		t.Fatalf("array child execution plan = %#v", items.ArrayElement)
	}
	values := layout.StructPlan.fields[1].layout
	if values.SliceElement == nil || !values.SliceElement.Prepared {
		t.Fatalf("slice child execution layout = %#v", values.SliceElement)
	}
}

func TestPreparedLayoutSharedProgramRunsAcrossInstanceClose(t *testing.T) {
	file, err := os.Open("../testdata/runtime/execution.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	decoder := json.NewDecoder(compressed)
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var image ir.ExecutionImage
	for decoder.More() {
		var vector struct {
			Name  string          `json:"name"`
			Image json.RawMessage `json:"image"`
		}
		if err := decoder.Decode(&vector); err != nil {
			t.Fatal(err)
		}
		if vector.Name == "nested_value_storage" {
			if err := json.Unmarshal(vector.Image, &image); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if image.Root == "" {
		t.Fatal("nested aggregate execution image not found")
	}
	program, err := LoadExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	first, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	first.supervisor.Load().stop()
	second, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	second.supervisor.Load().stop()
	firstExecution, err := first.Start("default")
	if err != nil {
		t.Fatal(err)
	}
	secondExecution, err := second.Start("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := firstExecution.PollSteps(1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := secondExecution.PollSteps(1); err != nil {
		t.Fatal(err)
	}
	for firstExecution.State() == ExecutionRunning {
		if _, _, err := firstExecution.PollSteps(128); err != nil {
			t.Fatal(err)
		}
	}
	firstResult, err := firstExecution.Result()
	if err != nil || len(firstResult.Values) != 1 {
		t.Fatalf("first shared-program result = %#v, %v", firstResult.Values, err)
	}
	if value, ok := firstResult.Values[0].Int64(); !ok || value != 42 {
		t.Fatalf("first shared-program value = %v, want 42", firstResult.Values)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for secondExecution.State() == ExecutionRunning {
		if _, _, err := secondExecution.PollSteps(128); err != nil {
			t.Fatal(err)
		}
	}
	secondResult, err := secondExecution.Result()
	if err != nil || len(secondResult.Values) != 1 {
		t.Fatalf("second shared-program result after first close = %#v, %v", secondResult.Values, err)
	}
	if value, ok := secondResult.Values[0].Int64(); !ok || value != 42 {
		t.Fatalf("second shared-program value = %v, want 42", secondResult.Values)
	}
}

func TestPreparedLayoutCutsRecursiveSliceChild(t *testing.T) {
	sliceRef := types.TypeRef{Kind: types.Slice, Node: "node.slice"}
	table := &types.TypeTable{Nodes: []types.TypeNode{{ID: "node.slice", Kind: types.Slice, Elem: sliceRef}}}
	if err := table.Reindex(); err != nil {
		t.Fatal(err)
	}
	typ := runtimeTypeWithTable(sliceRef, table)
	layout, err := buildPreparedValueLayout(typ, map[string]bool{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if layout.SliceElement != nil {
		t.Fatal("recursive slice expanded its child descriptor")
	}
}

func TestManagedDynamicTypeMetadataFollowsPayloadLifetime(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := module.importDetachedValue(newVMValue("Any", newVMValue("Int64", int64(42))), map[uintptr]guestRef{})
	if err != nil {
		t.Fatal(err)
	}
	layout, err := module.vm.valueLayouts.layoutFor(predeclaredRuntimeTypes["Int64"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := module.vm.valueLayouts.typeByID(layout.ID); !ok {
		t.Fatal("live dynamic payload lost its type metadata")
	}
	decoded, ok := value.payload().(vmValue)
	if !ok {
		t.Fatalf("dynamic payload = %T", value.payload())
	}
	if got, err := asInt64(decoded); err != nil || got != 42 {
		t.Fatalf("dynamic value after major = %d, %v", got, err)
	}
	if _, err := module.vm.heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	if len(module.vm.valueLayouts.types) != 0 || len(module.vm.valueLayouts.byType) != 0 || len(module.vm.valueLayouts.resident) != 0 {
		t.Fatal("unreachable dynamic type closure was retained")
	}
}

func TestManagedErrorInterfaceRetainsConcretePayloadAcrossCollection(t *testing.T) {
	module := newManagedValueTestModule()
	value, err := module.importDetachedValue(newVMValue("Error", newVMValue("String", "failure")), map[uintptr]guestRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, major := range []bool{false, true} {
		if _, err := module.vm.heap.collect([]guestRef{value.ref}, major); err != nil {
			t.Fatal(err)
		}
		concrete, ok := value.payload().(vmValue)
		if !ok || concrete.payload() != "failure" {
			t.Fatalf("error interface payload after collection = %#v", value.payload())
		}
	}
}

func TestPreparedLayoutResidentFailureDoesNotPublishMetadata(t *testing.T) {
	heap := newManagedHeap()
	heap.maxResidentBytes = 1
	registry := newPreparedLayoutRegistry()
	registry.heap = heap
	if _, err := registry.layoutFor(runtimeTypeFromText("struct{A:Int64,B:Slice<String>}")); !errors.Is(err, errManagedLimit) {
		t.Fatalf("layout admission error = %v", err)
	}
	if heap.stats().residentPageBytes != 0 || len(registry.byType) != 0 || len(registry.types) != 0 {
		t.Fatal("failed layout admission published metadata or residency")
	}
}

func TestPreparedLayoutConcurrentInterningUsesOneResidentAdmission(t *testing.T) {
	typ := runtimeTypeFromText("struct{A:Array<8,Int64>,B:Slice<String>,C:Any}")
	probe := newManagedValueTestModule()
	if _, err := probe.vm.valueLayouts.layoutFor(typ); err != nil {
		t.Fatal(err)
	}
	resident := probe.vm.heap.stats().residentPageBytes
	module := newManagedValueTestModule()
	module.vm.heap.maxResidentBytes = resident
	var group sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		group.Go(func() {
			<-start
			if _, err := module.vm.valueLayouts.layoutFor(typ); err != nil {
				t.Errorf("concurrent layout admission: %v", err)
			}
		})
	}
	close(start)
	group.Wait()
	if actual := module.vm.heap.stats().residentPageBytes; actual != resident {
		t.Fatalf("shared descriptor resident = %d, want %d", actual, resident)
	}
}

func TestPreparedLayoutTracesNestedPointerTypeIDs(t *testing.T) {
	layout, err := buildPreparedValueLayout(runtimeTypeFromText("struct{Values:Array<2,Ptr<Int64>>,Dynamic:Any}"), map[string]bool{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.TypeOffsets) != 2 || layout.TypeOffsets[0] != 16 || layout.TypeOffsets[1] != 40 || len(layout.Dynamics) != 1 || layout.Dynamics[0] != 48 {
		t.Fatalf("nested type lanes = %+v", layout)
	}
}

func TestManagedStructFieldMetadataFollowsParentLifetime(t *testing.T) {
	module := newManagedValueTestModule()
	inner := newRuntimeStructValue(module, "struct{Value:Int64}", map[string]vmValue{"Value": newVMValue("Int64", int64(42))})
	value := module.adoptValue(newRuntimeStructValue(module, "struct{Child:struct{Value:Int64},Text:String}", map[string]vmValue{
		"Child": inner, "Text": newVMValue("String", "retained"),
	}))
	if err := module.vm.takeAllocationFailure(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		child, err := loadFieldValue(module, value, "Child")
		if err != nil {
			t.Fatal(err)
		}
		answer, err := loadFieldValue(module, child, "Value")
		if err != nil {
			t.Fatal(err)
		}
		if actual, err := asInt64(answer); err != nil || actual != 42 {
			t.Fatalf("nested field = %d, %v", actual, err)
		}
		if _, err := module.vm.heap.collect([]guestRef{value.ref}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := module.vm.heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	if len(module.vm.valueLayouts.structLayouts) != 0 || len(module.vm.valueLayouts.resident) != 0 {
		t.Fatal("unreachable struct metadata retained its residency")
	}
}

func TestPreparedLayoutUsesSemanticPrimitiveWidths(t *testing.T) {
	registry := newPreparedLayoutRegistry()
	widths := ir.PrimitiveStorageBytes()
	for primitive := types.PrimitiveBool; primitive <= types.PrimitiveFunction; primitive++ {
		typ := runtimeTypeWithTable(types.Builtin(primitive), nil)
		layout, err := registry.layoutFor(typ)
		if err != nil {
			t.Fatalf("layout %s: %v", typ, err)
		}
		want := widths[primitive]
		if layout.Logical != want || uint64(layout.Stride) != want {
			t.Errorf("layout %s = logical %d stride %d, want %d", typ, layout.Logical, layout.Stride, want)
		}
		if layout.Primitive != primitive {
			t.Errorf("layout %s primitive = %d, want %d", typ, layout.Primitive, primitive)
		}
		if primitive >= types.PrimitiveInt && primitive <= types.PrimitiveComplex128 {
			info, ok := typ.NumericInfo()
			if !ok || layout.NumericKind != info.Kind || layout.NumericBits != uint16(info.Bits) {
				t.Errorf("layout %s numeric = (%d,%d), want (%d,%d)", typ, layout.NumericKind, layout.NumericBits, info.Kind, info.Bits)
			}
		}
		if primitive == types.PrimitiveError && (len(layout.Dynamics) != 1 || layout.Dynamics[0] != 0) {
			t.Errorf("error interface must trace its dynamic payload: %+v", layout)
		}
	}
}

func TestPreparedLayoutKeepsFingerprintAcrossTypeTables(t *testing.T) {
	registry := newPreparedLayoutRegistry()
	key := types.TypeKey{ModulePath: "layout/test", DeclID: "Counter"}
	ref := types.TypeRef{Kind: types.Named, Node: "counter", Named: key}
	var first preparedValueLayout
	for index, primitive := range []types.PrimitiveKind{types.PrimitiveInt64, types.PrimitiveInt64, types.PrimitiveInt8} {
		table := &types.TypeTable{}
		if err := table.Add(types.TypeNode{ID: ref.Node, Kind: types.Named, Identity: key, Underlying: types.Builtin(primitive)}); err != nil {
			t.Fatal(err)
		}
		layout, err := registry.layoutFor(runtimeTypeWithTable(ref, table))
		if primitive == types.PrimitiveInt8 {
			if err == nil {
				t.Fatal("conflicting storage shape accepted for a live type identity")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = layout
		} else if layout.ID != first.ID || layout.Stride != first.Stride {
			t.Fatal("equivalent type tables did not reuse their layout")
		}
	}
}

func TestPreparedLayoutRejectsSameWidthDifferentShape(t *testing.T) {
	registry := newPreparedLayoutRegistry()
	key := types.TypeKey{ModulePath: "layout/test", DeclID: "Scalar"}
	ref := types.TypeRef{Kind: types.Named, Node: "scalar", Named: key}
	for _, primitive := range []types.PrimitiveKind{types.PrimitiveInt64, types.PrimitiveFloat64} {
		table := &types.TypeTable{}
		if err := table.Add(types.TypeNode{ID: ref.Node, Kind: types.Named, Identity: key, Underlying: types.Builtin(primitive)}); err != nil {
			t.Fatal(err)
		}
		_, err := registry.layoutFor(runtimeTypeWithTable(ref, table))
		if primitive == types.PrimitiveInt64 && err != nil {
			t.Fatal(err)
		}
		if primitive == types.PrimitiveFloat64 && err == nil {
			t.Fatal("same-width named type changed physical shape without changing fingerprint")
		}
	}
}

func TestPreparedLayoutRejectsChangedInlineFieldSemantics(t *testing.T) {
	for _, changed := range []string{
		"struct{Left:Float64,Right:Int64}",
		"struct{Right:Int64,Left:Int64}",
		"struct{Left:Int64,Renamed:Int64}",
	} {
		t.Run(changed, func(t *testing.T) {
			registry := newPreparedLayoutRegistry()
			key := types.TypeKey{ModulePath: "layout/test", DeclID: "Pair"}
			for index, shape := range []string{"struct{Left:Int64,Right:Int64}", changed} {
				table := &types.TypeTable{}
				underlying, err := types.NewParser("layout/test", table).Parse(shape)
				if err != nil {
					t.Fatal(err)
				}
				node := types.TypeNode{ID: "pair", Kind: types.Named, Identity: key, Underlying: underlying}
				if err := table.Add(node); err != nil {
					t.Fatal(err)
				}
				_, err = registry.layoutFor(runtimeTypeWithTable(types.Ref(node), table))
				if index == 0 && err != nil {
					t.Fatal(err)
				}
				if index == 1 && err == nil {
					t.Fatal("equal-width fields changed meaning under a live layout identity")
				}
			}
		})
	}
}

func TestPreparedLayoutBindsNestedNamedInlineShape(t *testing.T) {
	registry := newPreparedLayoutRegistry()
	innerKey := types.TypeKey{ModulePath: "layout/test", DeclID: "Inner"}
	outerKey := types.TypeKey{ModulePath: "layout/test", DeclID: "Outer"}
	for index, primitive := range []types.PrimitiveKind{types.PrimitiveInt64, types.PrimitiveFloat64} {
		inner := types.TypeNode{ID: "inner", Kind: types.Named, Identity: innerKey, Underlying: types.Builtin(primitive)}
		array := types.TypeNode{ID: "array", Kind: types.Array, Length: 2, Elem: types.Ref(inner)}
		fields := types.TypeNode{ID: "fields", Kind: types.Struct, Fields: []types.Field{{Name: "Items", Type: types.Ref(array)}}}
		outer := types.TypeNode{ID: "outer", Kind: types.Named, Identity: outerKey, Underlying: types.Ref(fields)}
		table := types.NewTable(inner, array, fields, outer)
		_, err := registry.layoutFor(runtimeTypeWithTable(types.Ref(outer), table))
		if index == 0 && err != nil {
			t.Fatal(err)
		}
		if index == 1 && err == nil {
			t.Fatal("nested named inline representation changed under a live outer identity")
		}
	}
}

func TestStorageTypeClosureOwnsExternalTypesAndRebasesNodeIDs(t *testing.T) {
	key := types.TypeKey{ModulePath: "storage/external", DeclID: "Item"}
	externalRef := types.TypeRef{Kind: types.Named, Named: key}
	// Both modules use the same local node ID for different structures.
	external := types.NewTable(
		types.TypeNode{ID: "named", Kind: types.Named, Identity: key, Underlying: types.TypeRef{Kind: types.Struct, Node: "shape"}},
		types.TypeNode{ID: "shape", Kind: types.Struct, Fields: []types.Field{{Name: "Count", Type: types.Builtin(types.PrimitiveInt64)}, {Name: "Next", Type: types.TypeRef{Kind: types.Pointer, Node: "pointer"}}}},
		types.TypeNode{ID: "pointer", Kind: types.Pointer, Elem: externalRef},
	)
	rootRef := types.TypeRef{Kind: types.Struct, Node: "shape"}
	source := types.NewTable(
		types.TypeNode{ID: "shape", Kind: types.Struct, Fields: []types.Field{{Name: "Value", Type: externalRef}}},
		types.TypeNode{ID: "unrelated", Kind: types.Slice, Elem: types.Builtin(types.PrimitiveString)},
	)
	root := runtimeTypeWithTable(rootRef, source)
	owned, err := ownedStorageType(root, func(value any) vmType {
		typ := value.(vmType)
		if typ.Ref.Kind == types.Named && typ.Ref.Named == key {
			return runtimeTypeWithTable(externalRef, external)
		}
		return typ
	})
	if err != nil {
		t.Fatal(err)
	}
	if owned.Table == source || owned.Table == external {
		t.Fatal("storage type retained a source table")
	}
	before, err := buildPreparedValueLayout(root, map[string]bool{}, func(value any) vmType {
		typ := value.(vmType)
		if typ.Ref.Kind == types.Named {
			return runtimeTypeWithTable(externalRef, external)
		}
		return typ
	})
	if err != nil {
		t.Fatal(err)
	}
	// Change the input graph after ownership transfers. Live data metadata
	// must remain complete and must not observe these later source changes.
	external.Nodes[1].Fields[0].Type = types.Builtin(types.PrimitiveBool)
	source.Nodes[0].Fields[0].Name = "Changed"
	after, err := buildPreparedValueLayout(owned, map[string]bool{}, nil)
	if err != nil || !samePreparedLayout(before, after) {
		t.Fatalf("owned layout changed after source mutation: %+v, %+v, %v", before, after, err)
	}
	fields, ok := owned.StructFields()
	if !ok || fields[0].Name != "Value" {
		t.Fatalf("owned root fields = %+v", fields)
	}
	if _, found := owned.Table.Node(types.TypeRef{Kind: types.Slice, Node: "unrelated"}); found {
		t.Fatal("unrelated source type retained by data closure")
	}
}
