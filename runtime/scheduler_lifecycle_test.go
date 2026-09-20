package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func lifecycleArtifact(child []ir.Instruction) ir.Artifact {
	artifact := ir.NewArtifact("scheduler/lifecycle", "main")
	artifact.Globals = []ir.Global{{ID: "global.completed", Type: testType("Bool")}}
	entry := []ir.Instruction{
		{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.child"})},
		{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	}
	artifact.Functions = []ir.Function{
		{ID: "fn.entry", Signature: testSignature("function() Void"), Instructions: entry},
		{ID: "fn.main", Signature: testSignature("function() Void"), Instructions: append([]ir.Instruction(nil), entry...)},
		{ID: "fn.child", RevisionLocal: true, Signature: testSignature("function() Void"), Instructions: child},
	}
	return artifact
}

func delayedLifecycleChild(tail ...ir.Instruction) []ir.Instruction {
	instructions := make([]ir.Instruction, 0, taskInstructionQuantum*2+len(tail))
	for range taskInstructionQuantum * 2 {
		instructions = append(instructions,
			ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			ir.Instruction{Op: string(ir.OpPop)},
		)
	}
	return append(instructions, tail...)
}

func loopingLifecycleArtifact() ir.Artifact {
	artifact := ir.NewArtifact("scheduler/completed", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "loop"})},
			{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			{Op: string(ir.OpPop)},
			{Op: string(ir.OpJump), Payload: testPayload(ir.JumpPayload{Label: "loop"})},
		},
	}}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func TestInterruptHandleOnlyCancelsItsExecution(t *testing.T) {
	completedArtifact := ir.NewArtifact("scheduler/completed", "main")
	completedArtifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})}},
	}}
	completedArtifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	instance, err := patchTestProgram(t, completedArtifact, "completed-interrupt").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	first, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	stale := first.InterruptHandle()
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	plan, err := instance.PreparePatch(context.Background(), patchTestProgram(t, loopingLifecycleArtifact(), "loop-interrupt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	second, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	stale.Interrupt()
	if state, executed, err := second.pollSteps(context.Background(), 8, true); err != nil || state != ExecutionRunning || executed != 8 {
		t.Fatalf("execution after stale interrupt = %s, %d, %v", state, executed, err)
	}
	second.InterruptHandle().Interrupt()
	if _, err := second.Wait(context.Background()); second.State() != ExecutionCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("execution after own interrupt = %s, %v", second.State(), err)
	}
}

func TestCancelOneBackgroundScopeKeepsOtherScope(t *testing.T) {
	artifact := lifecycleArtifact([]ir.Instruction{
		{Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int>")})},
		{Op: string(ir.OpWaitableRecv)},
		{Op: string(ir.OpPop)},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	})
	instance, err := patchTestProgram(t, artifact, "independent-scopes").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	first, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.Cancel()
	stats, err := instance.RuntimeStats(context.Background())
	if err != nil || stats.ActiveScopes != 1 || stats.BlockedTasks != 1 {
		t.Fatalf("runtime after first scope cancel = %#v, %v", stats, err)
	}
	second.Cancel()
}

func TestLibraryInvocationLeavesSpawnedTaskRunning(t *testing.T) {
	artifact := lifecycleArtifact(delayedLifecycleChild(
		ir.Instruction{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.true"})},
		ir.Instruction{Op: string(ir.OpStoreGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.completed"})},
		ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	))
	artifact.Constants = []ir.Constant{{ID: "const.true", Type: testType("Bool"), Value: json.RawMessage(`true`)}}
	program := patchTestProgram(t, artifact, "library-background")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Call(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if err := instance.vm.enterOwnerContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		value := instance.vm.rootModule().state.globals["global.completed"].load()
		instance.vm.leaveOwner()
		if completed, _ := value.Data.(bool); completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spawned library task did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	if instance.Err() != nil || !instance.isOpen() {
		t.Fatalf("instance after background completion: state=%d err=%v", instance.lifecycleState(), instance.Err())
	}
}

func TestCancelCompletedRootReleasesBlockedBackgroundTask(t *testing.T) {
	artifact := lifecycleArtifact([]ir.Instruction{
		{Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int>")})},
		{Op: string(ir.OpWaitableRecv)},
		{Op: string(ir.OpPop)},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	})
	instance, err := patchTestProgram(t, artifact, "library-background-cancel").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, err := instance.RuntimeStats(context.Background())
	if err != nil || stats.ActiveScopes != 1 || stats.BlockedTasks != 1 || len(stats.BlockedContexts) != 1 {
		t.Fatalf("blocked background runtime = %#v, %v", stats, err)
	}
	if context := stats.BlockedContexts[0]; context.ScopeID != execution.scopeID || context.Reason != "channel selection is pending" {
		t.Fatalf("blocked background context = %#v", context)
	}
	execution.InterruptHandle().Interrupt()
	scope, err := execution.WaitScope(context.Background())
	if !errors.Is(err, context.Canceled) || scope.RootState != ExecutionCompleted || !scope.Done {
		t.Fatalf("canceled blocked scope = %#v, %v", scope, err)
	}
	if execution.State() != ExecutionCompleted {
		t.Fatalf("canceled root state = %s", execution.State())
	}
	stats, err = instance.RuntimeStats(context.Background())
	if err != nil || stats.ActiveScopes != 0 || stats.BlockedTasks != 0 {
		t.Fatalf("runtime after scope cancel = %#v, %v", stats, err)
	}
}

func TestLibraryBackgroundPanicFaultsInstance(t *testing.T) {
	artifact := lifecycleArtifact(delayedLifecycleChild(
		ir.Instruction{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.failure"})},
		ir.Instruction{Op: string(ir.OpPanic)},
	))
	artifact.Constants = []ir.Constant{{ID: "const.failure", Type: testType("String"), Value: json.RawMessage(`"background failed"`)}}
	program := patchTestProgram(t, artifact, "library-background-panic")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Call(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := instance.Wait(ctx); err == nil {
		t.Fatal("background panic did not fault instance")
	}
	if _, err := instance.Start("run"); !errors.Is(err, ErrInstanceFaulted) {
		t.Fatalf("start after background panic = %v", err)
	}
	if instance.State() != InstanceFaulted {
		t.Fatalf("instance state after background panic = %s", instance.State())
	}
}

func TestSpawnedTasksShareScopeStepBudget(t *testing.T) {
	artifact := lifecycleArtifact([]ir.Instruction{
		{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
		{Op: string(ir.OpPop)},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	})
	instance, err := patchTestProgram(t, artifact, "shared-scope-budget").Instantiate(context.Background(), InstanceOptions{Limits: Limits{MaxSteps: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(context.Background()); err == nil {
		t.Fatal("spawned task received an independent step budget")
	} else {
		var limit StepLimitError
		if !errors.As(err, &limit) || limit.MaxSteps != 2 {
			t.Fatalf("scope step limit = %T: %v", err, err)
		}
	}
}
