package runtime

import (
	"context"
	"testing"

	"github.com/d7z-team/mini-go/ffi"
)

func TestIdlePatchesReleaseRetiredRevisions(t *testing.T) {
	programs := []*Program{
		patchTestProgram(t, patchCallArtifact(10, 1), "idle-first"),
		patchTestProgram(t, patchCallArtifact(100, 2), "idle-second"),
	}
	instance, err := programs[0].Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	for n := range 10_000 {
		plan, err := instance.PreparePatch(context.Background(), programs[(n+1)%2])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := instance.ApplyPatch(plan); err != nil {
			t.Fatal(err)
		}
		if len(instance.vm.retiredRevisionSnapshot()) != 0 {
			t.Fatal("idle revision retained")
		}
	}
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 {
		t.Fatalf("retained revisions = %d, want 1", len(retained))
	}
}

func TestRetainedRevisionsDuringPendingPatchClose(t *testing.T) {
	first := patchTestProgram(t, patchCallArtifact(10, 1), "revision-old")
	second := patchTestProgram(t, patchCallArtifact(100, 2), "revision-new")
	instance, err := first.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	for range 100 {
		plan, err := instance.PreparePatch(context.Background(), second)
		if err != nil {
			t.Fatal(err)
		}
		closed := make(chan error, 1)
		go func() { closed <- plan.Close() }()
		snapshot, err := instance.RevisionRetention(context.Background())
		retained := snapshot.Revisions
		closeErr := <-closed
		if err != nil || closeErr != nil {
			t.Fatalf("snapshot = %v, close = %v", err, closeErr)
		}
		if len(retained) != 1 || retained[0].Revision.Generation != 1 || retained[0].Revision.Hash != "revision-old" {
			t.Fatalf("retained revisions = %#v", retained)
		}
		if snapshot.PendingTarget != nil && snapshot.PendingTarget.Hash != "revision-new" {
			t.Fatalf("pending target = %#v", snapshot.PendingTarget)
		}
	}
}

func TestPatchKeepsActiveFrameAndDispatchesNamedCallsToCurrentRevision(t *testing.T) {
	oldProgram := patchTestProgram(t, patchCallArtifact(10, 1), "revision-old")
	newProgram := patchTestProgram(t, patchCallArtifact(100, 2), "revision-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := instance.Close(); err != nil {
				t.Errorf("close instance: %v", err)
			}
		}
	})
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	state, executed, err := execution.PollSteps(1)
	if err != nil || state != ExecutionRunning || executed != 1 {
		t.Fatalf("first poll = %s, %d, %v", state, executed, err)
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	result, err := instance.ApplyPatch(plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Previous.Generation != 1 || result.Current.Generation != 2 || result.Current.Hash != "revision-new" {
		t.Fatalf("patch result = %#v", result)
	}
	if retained, err := instance.RetainedRevisions(context.Background()); err != nil || len(retained) != 2 {
		t.Fatalf("retained revisions = %#v", retained)
	}

	activeResult, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := activeResult.Values[0].Int64(); !ok || value != 12 {
		t.Fatalf("active result = %#v, want 12", activeResult.Values)
	}
	currentResult, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := currentResult.Values[0].Int64(); !ok || value != 102 {
		t.Fatalf("current result = %#v, want 102", currentResult.Values)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func TestRevisionLocalDirectCallUsesOldFrameRevision(t *testing.T) {
	oldProgram := patchTestProgram(t, patchRevisionLocalCallArtifact(10, 1, false), "revision-local-old")
	newProgram := patchTestProgram(t, patchRevisionLocalCallArtifact(100, 2, false), "revision-local-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, steps, pollErr := execution.PollSteps(1); pollErr != nil || state != ExecutionRunning || steps != 1 {
		t.Fatalf("initial poll = %s, %d, %v", state, steps, pollErr)
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if retained, err := instance.RetainedRevisions(context.Background()); err != nil || len(retained) != 2 {
		t.Fatalf("retained revisions while old frame runs = %#v, %v", retained, err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 11 {
		t.Fatalf("old-frame result = %#v, want 11", result.Values)
	}
	current, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := current.Values[0].Int64(); !ok || value != 102 {
		t.Fatalf("current revision result = %#v, want 102", current.Values)
	}
	if retained, err := instance.RetainedRevisions(context.Background()); err != nil || len(retained) != 1 || retained[0].Generation != 2 {
		t.Fatalf("retained revisions after old frame = %#v, %v", retained, err)
	}
}

func TestRevisionLocalDirectCallIgnoresPatchedFunctionOrder(t *testing.T) {
	oldProgram := patchTestProgram(t, patchRevisionLocalCallArtifact(10, 1, false), "revision-local-order-old")
	newProgram := patchTestProgram(t, patchRevisionLocalCallArtifact(100, 2, true), "revision-local-order-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, pollErr := execution.PollSteps(1); pollErr != nil || state != ExecutionRunning {
		t.Fatalf("initial poll = %s, %v", state, pollErr)
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 11 {
		t.Fatalf("old-frame result after reorder = %#v, want 11", result.Values)
	}
	current, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := current.Values[0].Int64(); !ok || value != 102 {
		t.Fatalf("reordered current result = %#v, want 102", current.Values)
	}
}

func TestRevisionLocalDirectCallIsolatedBetweenInstances(t *testing.T) {
	oldProgram := patchTestProgram(t, patchRevisionLocalCallArtifact(10, 1, false), "revision-local-isolated-old")
	newProgram := patchTestProgram(t, patchRevisionLocalCallArtifact(100, 2, false), "revision-local-isolated-new")
	first, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, first)
	second, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, second)
	plan, err := first.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	firstResult, err := first.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := second.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := firstResult.Values[0].Int64(); !ok || value != 102 {
		t.Fatalf("patched instance result = %#v, want 102", firstResult.Values)
	}
	if value, ok := secondResult.Values[0].Int64(); !ok || value != 11 {
		t.Fatalf("unpatched instance result = %#v, want 11", secondResult.Values)
	}
}

func TestTailCallFromOldFrameUsesCurrentRevisionAndReleasesOldPin(t *testing.T) {
	oldProgram := patchTestProgram(t, patchTailCallArtifact(1), "tail-old")
	newProgram := patchTestProgram(t, patchTailCallArtifact(2), "tail-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, pollErr := execution.PollSteps(1); pollErr != nil || state != ExecutionRunning {
		t.Fatalf("first poll = %s, %v", state, pollErr)
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 2 {
		t.Fatalf("tail call result = %#v, want current revision value 2", result.Values)
	}
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil || len(retained) != 1 || retained[0].Generation != 2 {
		t.Fatalf("retained revisions = %#v, %v", retained, err)
	}
}

func TestPatchAcrossThreeRevisionsRetainsOnlyActiveFrameCode(t *testing.T) {
	first := patchTestProgram(t, patchCallArtifact(10, 1), "revision-one")
	second := patchTestProgram(t, patchCallArtifact(100, 2), "revision-two")
	third := patchTestProgram(t, patchCallArtifact(1000, 3), "revision-three")
	instance, err := first.Instantiate(context.Background(), InstanceOptions{Limits: Limits{MaxRetainedRevisions: 3}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, pollErr := execution.PollSteps(1); pollErr != nil || state != ExecutionRunning {
		t.Fatalf("first poll = %s, %v", state, pollErr)
	}
	for _, program := range []*Program{second, third} {
		plan, prepareErr := instance.PreparePatch(context.Background(), program)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if _, applyErr := instance.ApplyPatch(plan); applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 2 || retained[0].Generation != 1 || retained[1].Generation != 3 {
		t.Fatalf("retained revisions during old frame = %#v", retained)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 13 {
		t.Fatalf("active result = %#v, want 13", result.Values)
	}
	retained, err = instance.RetainedRevisions(context.Background())
	if err != nil || len(retained) != 1 || retained[0].Generation != 3 {
		t.Fatalf("retained revisions after completion = %#v, %v", retained, err)
	}
}

func TestPatchKeepsPendingFFICallOnItsOriginalRevision(t *testing.T) {
	oldProgram := patchTestProgram(t, patchFFIArtifact(), "ffi-old")
	newProgram := patchTestProgram(t, patchFFIArtifact(), "ffi-new")
	started := make(chan ffi.Completion, 1)
	bridge := ffi.CallFunc(func(_ context.Context, request ffi.Request, complete ffi.Completion) (ffi.Call, error) {
		if request.Route != "patch" || len(request.Payload) != 0 {
			t.Fatalf("FFI request = %#v", request)
		}
		started <- complete
		return ffi.CancelFunc(func() {}), nil
	})
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, pollErr := execution.Poll(); pollErr != nil || state != ExecutionPending {
		t.Fatalf("FFI poll = %s, %v", state, pollErr)
	}
	completion := <-started
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if retained, err := instance.RetainedRevisions(context.Background()); err != nil || len(retained) != 2 {
		t.Fatalf("retained revisions with pending FFI = %#v, %v", retained, err)
	}
	roots, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil || !roots.Complete || len(roots.Roots) == 0 || roots.Roots[0].ScopeID != execution.scopeID {
		t.Fatalf("pending FFI roots = %+v, %v", roots, err)
	}
	completion(ffi.Result{Payload: []byte("old")})
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if payload, ok := result.Values[0].Bytes(); !ok || string(payload) != "old" {
		t.Fatalf("FFI result = %#v", result.Values)
	}
	if retained, err := instance.RetainedRevisions(context.Background()); err != nil || len(retained) != 1 || retained[0].Generation != 2 {
		t.Fatalf("retained revisions after FFI completion = %#v, %v", retained, err)
	}
}

func TestLastRevisionPinRemovesClosedRetiredRevision(t *testing.T) {
	machine := &vm{}
	revision := &instanceRevision{owner: machine}
	revision.retain()
	revision.retire(false)
	machine.addRetiredRevision(revision)
	if len(machine.retiredRevisionSnapshot()) != 1 {
		t.Fatal("pinned retired revision was not retained")
	}
	revision.release()
	if retained := machine.retiredRevisionSnapshot(); len(retained) != 0 {
		t.Fatalf("closed retired revisions = %#v", retained)
	}
}

func TestPatchKeepsExactClosureCode(t *testing.T) {
	oldProgram := patchTestProgram(t, patchClosureArtifact(3), "closure-old")
	newProgram := patchTestProgram(t, patchClosureArtifact(9), "closure-new")
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
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 3 {
		t.Fatalf("closure result = %#v, want 3", result.Values)
	}
}

func TestCanceledExecutionReleasesRetiredRevision(t *testing.T) {
	oldProgram := patchTestProgram(t, patchCallArtifact(10, 1), "cancel-old")
	newProgram := patchTestProgram(t, patchCallArtifact(100, 2), "cancel-new")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, pollErr := execution.PollSteps(1); pollErr != nil || state != ExecutionRunning {
		t.Fatalf("first poll = %s, %v", state, pollErr)
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if retained, err := instance.RetainedRevisions(context.Background()); err != nil || len(retained) != 2 {
		t.Fatalf("retained revisions before cancel = %#v", retained)
	}
	execution.Cancel()
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil {
		t.Fatalf("RetainedRevisions failed: %v", err)
	}
	if len(retained) != 1 || retained[0].Generation != 2 {
		t.Fatalf("retained revisions after cancel = %#v", retained)
	}
}

func TestCancelReleasesRetainedRevisionAndKeepsInstanceOpen(t *testing.T) {
	oldProgram := patchTestProgram(t, patchCallArtifact(10, 1), "limit-old")
	currentProgram := patchTestProgram(t, patchCallArtifact(100, 2), "limit-current")
	nextProgram := patchTestProgram(t, patchCallArtifact(1000, 3), "limit-next")
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{Limits: Limits{MaxRetainedRevisions: 2}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, pollErr := execution.PollSteps(1); pollErr != nil || state != ExecutionRunning {
		t.Fatalf("first poll = %s, %v", state, pollErr)
	}
	plan, err := instance.PreparePatch(context.Background(), currentProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.PreparePatch(context.Background(), nextProgram); patchErrorCode(err) != "resource_limit" {
		t.Fatalf("retained revision limit error = %v", err)
	}
	execution.Cancel()
	plan, err = instance.PreparePatch(context.Background(), nextProgram)
	if err != nil {
		t.Fatalf("prepare patch after cancellation: %v", err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatalf("apply patch after cancellation: %v", err)
	}
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil {
		t.Fatalf("RetainedRevisions failed: %v", err)
	}
	if len(retained) != 1 || retained[0].Generation != 3 {
		t.Fatalf("retained revisions after cancellation = %#v", retained)
	}
}
