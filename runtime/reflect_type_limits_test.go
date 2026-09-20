package runtime

import (
	"errors"
	"fmt"
	"testing"
)

func TestDynamicReflectionMetadataBudgets(t *testing.T) {
	for _, test := range []struct {
		name   string
		limits Limits
		code   string
	}{
		{"count", Limits{MaxDynamicTypes: 2}, "execution.dynamic_type_limit"},
		{"metadata", Limits{MaxDynamicTypeBytes: 1}, "execution.dynamic_type_bytes_limit"},
		{"allocation", Limits{MaxAllocatedBytes: 1}, "execution.allocation_limit"},
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
			if got := machine.refreshLiveGuestBytes(); got != machine.dynamicTypeBytes {
				t.Fatalf("metadata census = %d, want %d", got, machine.dynamicTypeBytes)
			}
		})
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
		if machine.refreshLiveGuestBytes() != machine.dynamicTypeBytes {
			t.Fatal("metadata census diverged")
		}
	})
}
