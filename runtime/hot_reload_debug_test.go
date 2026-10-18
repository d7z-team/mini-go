package runtime

import (
	"context"
	"testing"
)

func TestPatchReportsPinnedRevisionWhenOldFramePauses(t *testing.T) {
	oldProgram := patchTestProgram(t, patchCallArtifact(10, 1), "debug-old")
	newProgram := patchTestProgram(t, patchCallArtifact(100, 2), "debug-new")
	debugger := NewDebugger()
	instance, err := oldProgram.Instantiate(context.Background(), InstanceOptions{Debugger: debugger})
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
	if err := execution.RequestPause(); err != nil {
		t.Fatal(err)
	}
	state, err := execution.Poll()
	if state != ExecutionPaused || err != nil {
		t.Fatalf("pause poll = %s, %v", state, err)
	}
	event, ok := execution.PauseEvent()
	if !ok {
		t.Fatal("paused execution has no debug event")
	}
	if event.Generation != 1 || event.ProgramHash != "debug-old" || event.Frame.Generation != 1 || event.Frame.ProgramHash != "debug-old" {
		t.Fatalf("pause event revision = %#v", event)
	}
}

func TestPatchReportsPinnedRevisionWhenOldFramePanics(t *testing.T) {
	oldProgram := patchTestProgram(t, patchPanicArtifact("old panic"), "panic-old")
	newProgram := patchTestProgram(t, patchPanicArtifact("new panic"), "panic-new")
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
	if _, err := execution.Wait(context.Background()); err == nil {
		t.Fatal("old execution did not panic")
	}
	schemaError, ok := execution.DebugError()
	if !ok {
		t.Fatal("old execution has no schema error")
	}
	if schemaError.Generation != 1 || schemaError.ProgramHash != "panic-old" || len(schemaError.Stack) != 1 {
		t.Fatalf("panic revision = %#v", schemaError)
	}
	if frame := schemaError.Stack[0]; frame.Generation != 1 || frame.ProgramHash != "panic-old" {
		t.Fatalf("panic frame revision = %#v", frame)
	}
}
