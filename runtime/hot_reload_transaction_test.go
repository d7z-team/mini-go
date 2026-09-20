package runtime

import (
	"context"
	goruntime "runtime"
	"testing"
)

func TestPatchRejectsIncompatibleAndReusedPlansWithoutMutation(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "base")
	compatible := patchTestProgram(t, patchGlobalArtifact(2), "compatible")
	incompatibleArtifact := patchGlobalArtifact(2)
	incompatibleArtifact.Globals[0].Type = testType("String")
	incompatible := patchTestProgram(t, incompatibleArtifact, "incompatible")
	instance, err := base.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)

	if _, err := instance.PreparePatch(context.Background(), incompatible); patchErrorCode(err) != "global_shape_changed" {
		t.Fatalf("incompatible error = %v", err)
	}
	exportChangedArtifact := patchGlobalArtifact(2)
	exportChangedArtifact.Exports = nil
	exportChanged := patchTestProgram(t, exportChangedArtifact, "export-changed")
	if _, err := instance.PreparePatch(context.Background(), exportChanged); patchErrorCode(err) != "export_shape_changed" {
		t.Fatalf("export error = %v", err)
	}
	plan, err := instance.PreparePatch(context.Background(), compatible)
	if err != nil {
		t.Fatal(err)
	}
	instance.vm.rootModule().state.beginInitialization()
	if _, err := instance.ApplyPatch(plan); patchErrorCode(err) != "module_initializing" {
		t.Fatalf("initializing error = %v", err)
	}
	state := instance.vm.rootModule().state
	state.finishInitialization(nil)
	state.initState = moduleUninitialized
	plan, err = instance.PreparePatch(context.Background(), compatible)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.vm.enterOwnerContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() {
		_, err := instance.ApplyPatch(plan)
		applied <- err
	}()
	for instance.vm.controlWaiters.Load() == 0 {
		select {
		case err := <-applied:
			instance.vm.leaveOwner()
			t.Fatalf("patch crossed active owner boundary: %v", err)
		default:
			goruntime.Gosched()
		}
	}
	select {
	case err := <-applied:
		instance.vm.leaveOwner()
		t.Fatalf("patch completed before owner release: %v", err)
	default:
	}
	instance.vm.leaveOwner()
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	plan.mu.Lock()
	retainedOwner, retainedTarget, retainedModules, retainedBase := plan.owner, plan.target, plan.changedModules, plan.base
	plan.mu.Unlock()
	if retainedOwner != nil || retainedTarget != nil || retainedModules != nil || retainedBase != nil {
		t.Fatal("committed patch retained runtime state")
	}
	before := instance.Revision()
	if _, err := instance.ApplyPatch(plan); patchErrorCode(err) != "invalid_plan" {
		t.Fatalf("reused plan error = %v", err)
	}
	if after := instance.Revision(); after != before {
		t.Fatalf("reused plan changed revision from %#v to %#v", before, after)
	}
}

func TestPatchRejectsCapabilityNotInstalledByInstance(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "capability-base")
	base.code.image.Capabilities = []string{"console"}
	next := patchTestProgram(t, patchGlobalArtifact(2), "capability-next")
	next.code.image.Capabilities = []string{"console", "filesystem"}
	instance, err := base.Instantiate(context.Background(), InstanceOptions{FFI: &instanceLifecycleBridge{capabilities: []string{"console"}}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	before := instance.Revision()
	if _, err := instance.PreparePatch(context.Background(), next); patchErrorCode(err) != "capability_unavailable" {
		t.Fatalf("capability error = %v", err)
	}
	if after := instance.Revision(); after != before {
		t.Fatalf("failed capability patch changed revision: %#v -> %#v", before, after)
	}
}

func TestPreparePatchKeepsExistingPendingPlan(t *testing.T) {
	base := patchTestProgram(t, patchGlobalArtifact(1), "pending-base")
	next := patchTestProgram(t, patchGlobalArtifact(2), "pending-next")
	instance, err := base.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	plan, err := instance.PreparePatch(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PreparePatch(context.Background(), next); patchErrorCode(err) != "patch_pending" {
		t.Fatalf("second prepare = %v", err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatalf("first pending plan was damaged: %v", err)
	}
}
