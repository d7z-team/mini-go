package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestRevisionRootsByteArrayPointerRespectsBudget(t *testing.T) {
	program := patchTestProgram(t, patchGlobalArtifact(1), "byte-pointer-budget")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	for _, length := range []int{1024, 1024 * 1024} {
		if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		module := instance.vm.rootModule()
		source := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, length), length, length)
		pointer, ok, err := module.convertSliceToArrayPointer(source, runtimeTypeFromText(fmt.Sprintf("Ptr<Array<%d, Uint8>>", length)))
		if err != nil || !ok {
			t.Fatalf("conversion: %v %v", ok, err)
		}
		cell := module.state.globals["global.total"]
		cell.value, cell.initialized = pointer, true
		instance.vm.leaveOwner()
		goruntime.GC()
		var before, after goruntime.MemStats
		goruntime.ReadMemStats(&before)
		result, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{MaxNodes: 4})
		goruntime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Complete || result.ScannedNodes > 4 {
			t.Fatalf("byte storage inspection: %+v", result)
		}
		allocated := after.TotalAlloc - before.TotalAlloc
		t.Logf("backing=%d nodes=%d complete=%v allocated=%d", length, result.ScannedNodes, result.Complete, allocated)
		if allocated > 1024*1024 {
			t.Errorf("four-node inspection expanded a byte backing into %d allocated bytes", allocated)
		}
	}
}

func TestRevisionInspectionExplainsFramesClosuresAndPendingTarget(t *testing.T) {
	old := patchTestProgram(t, patchClosureArtifact(3), "roots-old")
	next := patchTestProgram(t, patchClosureArtifact(9), "roots-new")
	instance, err := old.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = execution.PollSteps(1); err != nil {
		t.Fatal(err)
	}
	plan, err := instance.PreparePatch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := instance.RevisionRetention(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Revisions) != 1 || summary.PendingTarget == nil || summary.PendingTarget.Hash != next.Hash() {
		t.Fatalf("summary: %+v", summary)
	}
	if _, err = instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	stats, err := instance.RuntimeStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	roots, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil {
		t.Fatal(err)
	}
	frame, closure := false, false
	for _, root := range roots.Roots {
		if root.ScopeID != execution.scopeID || root.TaskID == 0 {
			t.Fatalf("root: %+v", root)
		}
		frame = frame || root.Function == "fn.entry"
		closure = closure || root.Function == "fn.literal.1" && strings.Contains(root.Path, "stack")
	}
	if !roots.Complete || !frame || !closure {
		t.Fatalf("roots: %+v", roots)
	}
	after, err := instance.RuntimeStats(t.Context())
	if err != nil || !reflect.DeepEqual(stats, after) {
		t.Fatalf("inspection mutated accounting: %+v %+v %v", stats, after, err)
	}
	limited, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{MaxNodes: 1})
	if err != nil || limited.Complete || limited.ScannedNodes != 1 {
		t.Fatalf("limited: %+v %v", limited, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := instance.RevisionRoots(ctx, 1, RevisionRootLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err = execution.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	summary, err = instance.RevisionRetention(t.Context())
	if err != nil || len(summary.Revisions) != 1 || !summary.Revisions[0].Current || summary.Revisions[0].Revision.Generation != 2 {
		t.Fatalf("released: %+v %v", summary, err)
	}
}

func TestRevisionRootsBoundsCyclicAndSharedGlobalValues(t *testing.T) {
	program := patchTestProgram(t, patchGlobalArtifact(1), "global-roots")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	vm := instance.vm
	if err := vm.enterOwnerContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	module := vm.rootModule()
	cell := module.state.globals["global.total"]
	ref := functionRef{FunctionID: "fn.entry", exact: module, upvalues: map[string]*slot{"cycle": cell}}
	cell.value = newVMValue("Any", []vmValue{newVMValue("Function", ref), newVMValue("Function", ref)})
	cell.initialized = true
	vm.leaveOwner()
	full, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil || !full.Complete {
		t.Fatalf("cyclic: %+v %v", full, err)
	}
	found := false
	for _, root := range full.Roots {
		found = found || strings.HasPrefix(root.Path, "global ") && root.Function == "fn.entry"
	}
	if !found {
		t.Fatalf("global: %+v", full)
	}
	for _, limits := range []RevisionRootLimits{{MaxDepth: 1}, {MaxRoots: 1}, {MaxNodes: 3}} {
		out, err := instance.RevisionRoots(t.Context(), 1, limits)
		if err != nil || out.Complete {
			t.Fatalf("limit %+v: %+v %v", limits, out, err)
		}
	}
}

func TestRevisionRootsGlobalReferenceReleasesRetiredCodeNaturally(t *testing.T) {
	artifact := patchGlobalArtifact(1)
	artifact.Globals[0].Type = testType("Any")
	base := patchTestProgram(t, artifact, "global-pin-old")
	next := patchTestProgram(t, artifact, "global-pin-new")
	instance, err := base.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	module := instance.vm.rootModule()
	cell := module.state.globals["global.total"]
	cell.value = newVMValue("Function", functionRef{FunctionID: "fn.entry", exact: module})
	cell.initialized = true
	instance.vm.leaveOwner()
	plan, err := instance.PreparePatch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	summary, err := instance.RevisionRetention(t.Context())
	if err != nil || len(summary.Revisions) != 2 || !summary.Revisions[0].GlobalRoot || summary.Revisions[0].Pins != 0 {
		t.Fatalf("global pin: %+v %v", summary, err)
	}
	roots, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil || !roots.Complete || len(roots.Roots) != 1 || !strings.HasPrefix(roots.Roots[0].Path, "global ") {
		t.Fatalf("global roots: %+v %v", roots, err)
	}
	if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	cell.value = newVMValue("Any", nil)
	instance.vm.sweepRetiredRevisions()
	instance.vm.leaveOwner()
	summary, err = instance.RevisionRetention(t.Context())
	if err != nil || len(summary.Revisions) != 1 || summary.Revisions[0].Revision.Generation != 2 {
		t.Fatalf("global released: %+v %v", summary, err)
	}
}
