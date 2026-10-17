package runtime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestDynamicReflectionMetadataBudgets(t *testing.T) {
	for _, test := range []struct {
		name   string
		limits Limits
		code   string
	}{
		{"count", Limits{MaxDynamicTypes: 2}, "execution.dynamic_type_limit"},
		{"metadata", Limits{MaxDynamicTypeBytes: 1}, "execution.dynamic_type_bytes_limit"},
		{"allocation", Limits{MaxGuestBytes: 1}, "execution.allocation_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			machine := &vm{limits: normalizeLimits(test.limits)}
			for i := 0; ; i++ {
				beforeCount, beforeBytes := machine.dynamicTypeCount, machine.dynamicTypeBytes
				_, err := reflectRegisterDynamicType(intrinsicContext{vm: machine}, fmt.Sprintf("Array<%d,Int>", i), TypeInfo{})
				if err != nil {
					var limit ResourceLimitError
					if !errors.As(err, &limit) || limit.Code != test.code {
						t.Fatalf("error = %v", err)
					}
					if machine.dynamicTypeCount != beforeCount || machine.dynamicTypeBytes != beforeBytes || len(machine.reflectTypes.snapshot()) != beforeCount {
						t.Fatal("failed registration partially committed")
					}
					break
				}
				if i > 2 {
					t.Fatal("type budget did not stop growth")
				}
			}
		})
	}
}

func TestDynamicReflectionMetadataFollowsManagedTypeOwner(t *testing.T) {
	module := newManagedValueTestModule()
	vm := module.vm
	layout := managedTestLayout(t, vm.heap, managedLayout{stride: 8})
	reservation, err := vm.heap.reserve(layout, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	owned := vmValue{Type: coerceRuntimeType("reflect.Type"), ref: reservation.commit(), heap: vm.heap}
	vm.dynamicTypeCount, vm.dynamicTypeBytes = 1, 10
	vm.reflectTypes.store("Array<7,Int>", TypeInfo{Key: "Array<7,Int>"})
	vm.recordDynamicTypeCost("Array<7,Int>", 10)
	vm.trackReflectTypeOwner(owned, "Array<7,Int>")
	vm.controlRoots = []vmValue{owned}
	if err := vm.collectMajorManaged(); err != nil {
		t.Fatal(err)
	}
	if vm.dynamicTypeCount != 1 || len(vm.reflectTypes.snapshot()) != 1 {
		t.Fatalf("live type metadata was reclaimed: count=%d cache=%d", vm.dynamicTypeCount, len(vm.reflectTypes.snapshot()))
	}
	vm.controlRoots = nil
	if err := vm.collectMajorManaged(); err != nil {
		t.Fatal(err)
	}
	if vm.dynamicTypeCount != 0 || vm.dynamicTypeBytes != 0 || len(vm.reflectTypes.snapshot()) != 0 {
		t.Fatalf("dead type metadata retained: count=%d bytes=%d cache=%d", vm.dynamicTypeCount, vm.dynamicTypeBytes, len(vm.reflectTypes.snapshot()))
	}
	if stats := vm.heap.stats(); stats.occupiedBytes != 0 || stats.reservedBytes != 0 {
		t.Fatalf("dead reflection owner retained ledger bytes: %+v", stats)
	}
}

func TestUnpublishedDynamicReflectionMetadataIsReclaimedAtCollection(t *testing.T) {
	vm := &vm{limits: normalizeLimits(Limits{MaxDynamicTypes: 4, MaxDynamicTypeBytes: 1 << 20}), heap: newManagedHeap()}
	vm.heap.onReclaim = vm.reclaimManagedMetadata
	if _, err := reflectRegisterDynamicType(intrinsicContext{vm: vm}, "Array<19,Int>", TypeInfo{}); err != nil {
		t.Fatal(err)
	}
	if vm.dynamicTypeCount != 1 || len(vm.reflectTypes.snapshot()) != 1 {
		t.Fatalf("dynamic type was not registered: count=%d cache=%d", vm.dynamicTypeCount, len(vm.reflectTypes.snapshot()))
	}
	if err := vm.collectMajorManaged(); err != nil {
		t.Fatal(err)
	}
	if vm.dynamicTypeCount != 0 || vm.dynamicTypeBytes != 0 || len(vm.reflectTypes.snapshot()) != 0 || len(vm.reflectTypeValues.snapshot()) != 0 {
		t.Fatalf("unpublished metadata retained after collection: count=%d bytes=%d types=%d values=%d", vm.dynamicTypeCount, vm.dynamicTypeBytes, len(vm.reflectTypes.snapshot()), len(vm.reflectTypeValues.snapshot()))
	}
}

func TestInlineReflectTypeTracksConcreteManagedOwner(t *testing.T) {
	module := newManagedValueTestModule()
	info := TypeInfo{Key: "Array<23,Int>", Kind: "array", Type: "Array<23,Int>"}
	if _, err := reflectRegisterDynamicType(intrinsicContext{vm: module.vm, module: module}, info.Key, info); err != nil {
		t.Fatal(err)
	}
	// Use standalone structural views so this focused test does not require a
	// complete stdlib module registry. The runtime path sees the same interface
	// dynamic slot and concrete runtimeType record as the real reflect package.
	table := &types.TypeTable{Nodes: []types.TypeNode{
		{ID: "reflect.runtimeType.inline", Kind: types.Struct, Fields: []types.Field{{Name: "key", Type: types.Builtin(types.PrimitiveString)}}},
	}}
	if err := table.Reindex(); err != nil {
		t.Fatal(err)
	}
	innerType := runtimeTypeWithTable(types.TypeRef{Kind: types.Struct, Node: "reflect.runtimeType.inline"}, table)
	innerType.text = "reflect.runtimeType"
	typeValue := vmValue{
		Type: vmType{Ref: types.TypeRef{Kind: types.Interface, Node: "reflect.Type.inline"}, Table: table, text: "reflect.Type"},
		Data: newRuntimeStructValue(nil, innerType, map[string]vmValue{"key": newVMValue("String", info.Key)}),
	}
	layout, err := module.vm.valueLayouts.layoutFor(typeValue.Type)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, layout.Stride)
	if err := module.writeInlineValue(raw, typeValue, map[uintptr]guestRef{}); err != nil {
		t.Fatal(err)
	}
	module.vm.reflectMetadataMu.Lock()
	owners := make([]guestRef, 0, len(module.vm.reflectTypeOwners))
	for ref := range module.vm.reflectTypeOwners {
		owners = append(owners, ref)
	}
	module.vm.reflectMetadataMu.Unlock()
	if len(owners) != 1 || !owners[0].valid() {
		t.Fatalf("inline reflect.Type owner tickets = %v", owners)
	}
	if _, err := module.vm.heap.collect(owners, true); err != nil {
		t.Fatal(err)
	}
	if module.vm.dynamicTypeCount != 1 {
		t.Fatalf("rooted concrete reflect.Type was reclaimed: count=%d", module.vm.dynamicTypeCount)
	}
	if _, err := module.vm.heap.collect(nil, true); err != nil {
		t.Fatal(err)
	}
	module.vm.drainReflectMetadataReleases()
	if module.vm.dynamicTypeCount != 0 {
		t.Fatalf("dead inline reflect.Type metadata retained: count=%d", module.vm.dynamicTypeCount)
	}
}

func TestReflectionConstructorsUseTypeBudget(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(intrinsicContext, []vmValue) ([]vmValue, error)
		args func(vmValue) []vmValue
	}{
		{"array", reflectArrayOf, func(typ vmValue) []vmValue { return []vmValue{newVMValue("Int", int64(3)), typ} }},
		{"channel", reflectChanOf, func(typ vmValue) []vmValue { return []vmValue{newVMValue("Int", int64(3)), typ} }},
		{"function", reflectFuncOf, func(typ vmValue) []vmValue {
			return []vmValue{newSliceValue("Slice<reflect.Type>", []vmValue{typ}), newSliceValue("Slice<reflect.Type>", nil), newBoolValue(false)}
		}},
		{"struct", reflectStructOf, func(vmValue) []vmValue { return []vmValue{newSliceValue("Slice<reflect.StructField>", nil)} }},
		{"pointer", reflectTypePointerTo, func(typ vmValue) []vmValue { return []vmValue{typ} }},
		{"slice", reflectTypeSliceOf, func(typ vmValue) []vmValue { return []vmValue{typ} }},
		{"map", reflectTypeMapOf, func(typ vmValue) []vmValue { return []vmValue{typ, typ} }},
		{"new", reflectNew, func(typ vmValue) []vmValue { return []vmValue{typ} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			machine := &vm{limits: normalizeLimits(Limits{MaxDynamicTypes: 1})}
			ctx := intrinsicContext{vm: machine}
			if _, err := reflectRegisterDynamicType(ctx, "Array<42,Int>", TypeInfo{}); err != nil {
				t.Fatal(err)
			}
			info := reflectTypeInfoForTypeText(nil, "Int")
			typ := reflectTypeValueFromVM(machine, info)
			_, err := test.call(ctx, test.args(typ))
			var limit ResourceLimitError
			if !errors.As(err, &limit) || limit.Code != "execution.dynamic_type_limit" {
				t.Fatalf("constructor error = %v", err)
			}
		})
	}
}

func TestDynamicReflectionTypeIdentitySurvivesPatch(t *testing.T) {
	first := patchTestProgram(t, patchCallArtifact(10, 1), "reflection-old")
	second := patchTestProgram(t, patchCallArtifact(100, 2), "reflection-new")
	instance, err := first.Instantiate(t.Context(), InstanceOptions{Limits: Limits{MaxDynamicTypes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	machine := instance.vm
	ctx := intrinsicContext{vm: machine}
	if _, err := reflectRegisterDynamicType(ctx, "Map<Int,String>", TypeInfo{}); err != nil {
		t.Fatal(err)
	}
	bytes := machine.dynamicTypeBytes
	plan, err := instance.PreparePatch(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := reflectRegisterDynamicType(ctx, "Map<Int, String>", TypeInfo{}); err != nil {
		t.Fatal(err)
	}
	if machine.dynamicTypeCount != 1 || machine.dynamicTypeBytes != bytes {
		t.Fatal("patch or repeated spelling changed type accounting")
	}
	stats, err := instance.RuntimeStats(t.Context())
	if err != nil || stats.DynamicTypes != 1 || stats.DynamicTypeBytes != bytes {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if machine.dynamicTypeCount != 0 || machine.dynamicTypeBytes != 0 || len(machine.reflectTypes.snapshot()) != 0 {
		t.Fatal("close retained metadata")
	}
}

func FuzzDynamicReflectionTypeBudget(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 0, 8, 9})
	f.Fuzz(func(t *testing.T, lengths []byte) {
		if len(lengths) > 128 {
			lengths = lengths[:128]
		}
		machine := &vm{limits: normalizeLimits(Limits{MaxDynamicTypes: 8})}
		seen := make(map[byte]bool)
		for _, length := range lengths {
			_, err := reflectRegisterDynamicType(intrinsicContext{vm: machine}, fmt.Sprintf("Array<%d,Int>", length), TypeInfo{})
			if seen[length] || len(seen) < 8 {
				if err != nil {
					t.Fatal(err)
				}
				seen[length] = true
			} else {
				var limit ResourceLimitError
				if !errors.As(err, &limit) || limit.Code != "execution.dynamic_type_limit" {
					t.Fatalf("error = %v", err)
				}
			}
			if len(seen) != machine.dynamicTypeCount || len(machine.reflectTypes.snapshot()) != len(seen) {
				t.Fatal("registration identity/accounting diverged")
			}
		}
	})
}
