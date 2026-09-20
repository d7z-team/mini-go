package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDebugSnapshotPreservesPausedTaskScope(t *testing.T) {
	child := delayedLifecycleChild(ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})})
	artifact := lifecycleArtifact(child)
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.child", pc: len(child) - 1, line: 10, column: 2})
	program := patchTestProgram(t, artifact, "cross-scope-debug")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{Debugger: NewDebugger()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.SetBreakpoints(artifact.Module.Path, "main.mgo", []int{10}); err != nil {
		t.Fatal(err)
	}
	first, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	// Hold the owner so the supervisor cannot race the deterministic interleaving.
	if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer instance.vm.leaveOwner()
	outcome := instance.vm.runPrepared(defaultPollQuantum)
	if outcome.state != ExecutionCompleted {
		t.Fatalf("first root: %+v", outcome)
	}
	first.capture(outcome)
	scopeID, err := instance.vm.prepareFunction("fn.child", nil)
	if err != nil {
		t.Fatal(err)
	}
	second := newExecution(instance, instance.vm.profileOptions)
	second.scopeID = scopeID
	instance.vm.machine.attachExecution(scopeID, second)
	instance.active.Store(second)
	outcome = instance.vm.runPrepared(defaultPollQuantum)
	if outcome.state != ExecutionPaused {
		t.Fatalf("second poll: %+v", outcome)
	}
	second.capture(outcome)
	paused := instance.vm.machine.paused
	snapshot, err := second.DebugSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if paused.scope.id != first.scopeID {
		t.Fatalf("expected older task, actual scope=%d", paused.scope.id)
	}
	if len(snapshot.Threads) == 0 || len(snapshot.Frames) == 0 {
		t.Fatalf("missing paused task: %+v", snapshot)
	}
	for _, thread := range snapshot.Threads {
		if thread.ScopeID != first.scopeID {
			t.Errorf("thread scope=%d want=%d", thread.ScopeID, first.scopeID)
		}
	}
	for _, frame := range snapshot.Frames {
		if frame.ScopeID != first.scopeID {
			t.Errorf("frame scope=%d want=%d", frame.ScopeID, first.scopeID)
		}
	}
}

func TestLibraryBackgroundTaskCanBeInspectedAndContinued(t *testing.T) {
	child := delayedLifecycleChild(ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})})
	artifact := lifecycleArtifact(child)
	setTestInstructionLocations(t, &artifact, testInstructionLocation{function: "fn.child", pc: len(child) - 1, line: 10, column: 2})
	program := patchTestProgram(t, artifact, "library-background-debug")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{Debugger: NewDebugger()})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	resolved, err := instance.SetBreakpoints(artifact.Module.Path, "main.mgo", []int{10})
	if err != nil || len(resolved) != 1 || !resolved[0].Verified {
		t.Fatalf("background breakpoint = %#v, %v", resolved, err)
	}
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-execution.Updates():
	case <-time.After(time.Second):
		t.Fatal("background breakpoint did not publish an update")
	}
	snapshot, err := execution.DebugSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Threads) == 0 || len(snapshot.Frames) == 0 || snapshot.Frames[0].FunctionID != "fn.child" {
		t.Fatalf("background debug snapshot = %#v", snapshot)
	}
	if _, err := execution.Continue(); err != nil {
		t.Fatal(err)
	}
	if execution.State() != ExecutionCompleted || !instance.isOpen() {
		t.Fatalf("continued background task changed invocation state: execution=%s instance=%d", execution.State(), instance.lifecycleState())
	}
	if scope, err := execution.WaitScope(context.Background()); err != nil || !scope.Done {
		t.Fatalf("continued background scope = %#v, %v", scope, err)
	}
	canceled, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := canceled.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled.Updates():
	case <-time.After(time.Second):
		t.Fatal("second background breakpoint did not publish an update")
	}
	if _, err := canceled.DebugSnapshot(); err != nil {
		t.Fatal(err)
	}
	canceled.Cancel()
	if _, err := canceled.WaitScope(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled paused scope = %v", err)
	}
	if _, err := canceled.DebugSnapshot(); err == nil {
		t.Fatal("canceled scope retained its debug inspection")
	}
}
