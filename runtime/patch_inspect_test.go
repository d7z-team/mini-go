package runtime

import (
	"reflect"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPatchInspectionReportsConstantsCapabilitiesAndIncompatibleContracts(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "inspect-base")
	next := patchTestProgram(t, patchGlobalArtifact(2), "inspect-next")
	base.code.image.Capabilities = []string{"old", "shared"}
	next.code.image.Capabilities = []string{"new", "shared"}
	report, err := ComparePrograms(base, next)
	if err != nil || !report.Compatible || len(report.Modules) != 1 || !report.Modules[0].ConstantsChanged || len(report.Modules[0].Functions) != 0 {
		t.Fatalf("constants: %+v %v", report, err)
	}
	if !reflect.DeepEqual(report.AddedCapabilities, []string{"new"}) || !reflect.DeepEqual(report.RemovedCapabilities, []string{"old"}) {
		t.Fatalf("capabilities: %+v", report)
	}
	artifact := patchGlobalArtifact(2)
	artifact.Globals[0].Type = testType("String")
	artifact.Functions[0].Code = testSlotCode([]string{"Int64"}, []ir.Instruction{{Op: ir.OpZero, Payload: testTypePayload("Int64")}, {Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}}}, [][2][]uint32{{nil, {0}}, {{0}, nil}})
	incompatible := patchTestProgram(t, artifact, "inspect-incompatible")
	report, err = ComparePrograms(base, incompatible)
	if err != nil || report.Compatible || report.Rejection == nil || report.Rejection.Code != "global_shape_changed" || len(report.Modules[0].Globals) != 1 {
		t.Fatalf("incompatible: %+v %v", report, err)
	}
	for range 10 {
		again, err := ComparePrograms(base, incompatible)
		if err != nil || !reflect.DeepEqual(report, again) {
			t.Fatalf("unstable report: %+v %v", again, err)
		}
	}
}

func TestPatchInspectionNamesDeclarationAndEntryChanges(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "declarations-base")
	artifact := patchGlobalArtifact(1)
	insertTestDelay(artifact.Functions[0].Code, 0, 1)
	artifact.Functions = append(artifact.Functions, ir.Function{ID: "fn.extra", Signature: testSignature("function() Void"), Code: testSlotCode([]string{}, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}})})
	artifact.Exports = append(artifact.Exports, ir.Export{Name: "Extra", Kind: "function", ID: "fn.extra"})
	next := patchTestProgram(t, artifact, "declarations-next")
	next.code.image.Entries = append(next.code.image.Entries, ir.Entry{Name: "extra", ModulePath: artifact.Module.Path, FunctionID: "fn.extra"})
	report, err := ComparePrograms(base, next)
	if err != nil || !report.Compatible || len(report.Modules) != 1 {
		t.Fatalf("report: %+v %v", report, err)
	}
	module := report.Modules[0]
	want := []DeclarationChange{{Name: "fn.entry", Kind: "modified"}, {Name: "fn.extra", Kind: "added"}}
	if !reflect.DeepEqual(module.Functions, want) || !reflect.DeepEqual(module.Exports, []DeclarationChange{{Name: "Extra", Kind: "added"}}) || !reflect.DeepEqual(report.Entries, []DeclarationChange{{Name: "extra", Kind: "added"}}) {
		t.Fatalf("declarations: %+v", report)
	}
	reverse, err := ComparePrograms(next, base)
	if err != nil || reverse.Compatible || reverse.Rejection == nil || reverse.Modules[0].Functions[1].Kind != "removed" || reverse.Entries[0].Kind != "removed" {
		t.Fatalf("removed: %+v %v", reverse, err)
	}
}

func TestPatchPlanInspectionCopiesInputsAndReportsSymbolOnlyChanges(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "inspect-symbols")
	next := base.WithoutSymbols()
	report, err := ComparePrograms(base, next)
	if err != nil || !report.Compatible || !report.SymbolsChanged || len(report.Modules) != 0 {
		t.Fatalf("symbols: %+v %v", report, err)
	}
	instance, err := base.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	plan, err := instance.PreparePatch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	report, err = plan.Inspect()
	if err != nil || report.BaseGeneration != 1 || report.Base.Hash != base.Hash() {
		t.Fatalf("plan: %+v %v", report, err)
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
	if plan.base != nil || plan.target != nil {
		t.Fatal("closed plan retained programs")
	}
	if report.Target.Hash != next.Hash() {
		t.Fatal("report changed after close")
	}
	if _, err := plan.Inspect(); err == nil {
		t.Fatal("inspection accepted closed plan")
	}
}
