package runtime

import (
	"context"
	"errors"
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

func TestStepIntoCodeOnlyRevisionPausesAtProgramCounter(t *testing.T) {
	oldArtifact := patchCallArtifact(10, 1)
	callPC := len(oldArtifact.Functions[0].Code.Instructions) - 3
	setTestInstructionLocations(t, &oldArtifact, testInstructionLocation{function: "fn.entry", pc: callPC, line: 8, column: 1})
	oldProgram := patchTestProgram(t, oldArtifact, "mixed-step-old")
	newProgram := patchTestProgram(t, patchCallArtifact(100, 2), "mixed-step-new").WithoutSymbols()
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
		t.Fatalf("initial poll = %s, %v", state, err)
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
	if state, err := execution.Poll(); err != nil || state != ExecutionPaused {
		t.Fatalf("pause poll = %s, %v", state, err)
	}
	if _, err := execution.StepInto(); err != nil {
		t.Fatal(err)
	}
	for polls := 0; polls < 4 && execution.State() == ExecutionRunning; polls++ {
		if _, err := execution.Poll(); err != nil {
			t.Fatal(err)
		}
	}
	if execution.State() != ExecutionPaused {
		t.Fatalf("step to call state = %s", execution.State())
	}
	if _, err := execution.StepInto(); err != nil {
		t.Fatal(err)
	}
	for polls := 0; polls < 4 && execution.State() == ExecutionRunning; polls++ {
		if _, err := execution.Poll(); err != nil {
			t.Fatal(err)
		}
	}
	if execution.State() != ExecutionPaused {
		t.Fatalf("step into code-only frame state = %s", execution.State())
	}
	snapshot, err := execution.DebugSnapshot()
	if err != nil || len(snapshot.Frames) == 0 || snapshot.Frames[0].Generation != 2 || snapshot.Frames[0].HasSymbols || snapshot.Frames[0].FunctionID != "fn.value" {
		t.Fatalf("code-only step snapshot = %#v, %v", snapshot, err)
	}
	if _, err := execution.StepInto(); !errors.Is(err, ErrDebugSymbolsUnavailable) || execution.State() != ExecutionPaused {
		t.Fatalf("second code-only step = state %s, error %v", execution.State(), err)
	}
	if _, err := execution.Continue(); err != nil {
		t.Fatal(err)
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
