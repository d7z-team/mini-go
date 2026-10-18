package runtime

import (
	"context"
	"testing"
)

func TestPatchPreservesGlobalSlotsAndDoesNotRunInitAgain(t *testing.T) {
	oldProgram := patchTestProgram(t, patchGlobalArtifact(1), "global-old")
	newProgram := patchTestProgram(t, patchGlobalArtifact(2), "global-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)

	first, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := first.Values[0].Int64(); !ok || value != 1 {
		t.Fatalf("first result = %#v", first.Values)
	}
	oldCell := instance.vm.rootModule().state.globals["global.total"]
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if instance.vm.rootModule().state.globals["global.total"] != oldCell {
		t.Fatal("patch replaced the global slot")
	}
	second, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := second.Values[0].Int64(); !ok || value != 3 {
		t.Fatalf("second result = %#v, want 3", second.Values)
	}
}

func TestPatchCommitsCompleteModuleGraphAtomically(t *testing.T) {
	oldProgram := patchMultiModuleProgram(t, patchMultiRootArtifact(1), patchMultiDependencyArtifact(10), "multi-old")
	newProgram := patchMultiModuleProgram(t, patchMultiRootArtifact(2), patchMultiDependencyArtifact(20), "multi-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, err := execution.PollSteps(1); err != nil || state != ExecutionRunning {
		t.Fatalf("first poll = %s, %v", state, err)
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	result, err := instance.ApplyPatch(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedModules) != 2 || result.ChangedModules[0] != "patch/dep" || result.ChangedModules[1] != "patch/root" {
		t.Fatalf("changed modules = %#v", result.ChangedModules)
	}
	active, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := active.Values[0].Int64(); !ok || value != 11 {
		t.Fatalf("active result = %#v, want 11", active.Values)
	}
	fresh, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := fresh.Values[0].Int64(); !ok || value != 22 {
		t.Fatalf("fresh result = %#v, want 22", fresh.Values)
	}
}
