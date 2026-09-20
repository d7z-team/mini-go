package runtime

import (
	"context"
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestControlRequestReturnsTaskAtInstructionBoundaryWithoutConsumingBudget(t *testing.T) {
	artifact := ir.NewArtifact("scheduler/control-handoff", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.main", Signature: testSignature("function()"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			{Op: string(ir.OpPop)},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
		},
	}}
	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vm.closeRevisions)
	if _, err := vm.prepareFunction("fn.main", nil); err != nil {
		t.Fatal(err)
	}
	machine := vm.machine
	task := machine.popRunnable()
	if !task.ownership.acquire() {
		t.Fatal("task did not acquire execution lease")
	}
	vm.controlWaiters.Add(1)
	slice := taskSlice{limit: 3}
	yield, _, err := machine.runTask(task, &slice)
	task.ownership.relinquish()
	if err != nil || yield.kind != taskYieldPoll || slice.executed != 0 || task.frames[0].frame.pc != 0 {
		t.Fatalf("control handoff consumed an instruction: yield=%v slice=%+v err=%v", yield, slice, err)
	}
	vm.controlWaiters.Add(-1)
	machine.pushRunnable(task)
	outcome := vm.runPrepared(3)
	if outcome.state != ExecutionCompleted || outcome.executed != 3 {
		t.Fatalf("resumed outcome=%+v", outcome)
	}
}

func TestCanceledControlRequestLeavesOwnershipAndDispatchAvailable(t *testing.T) {
	vm := &vm{ownerWake: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := vm.enterOwnerContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled control request: %v", err)
	}
	if vm.owner.Load() || vm.controlWaiters.Load() != 0 {
		t.Fatal("canceled request retained ownership or stopped dispatch")
	}
	if err := vm.enterOwner(); err != nil {
		t.Fatal(err)
	}
	vm.leaveOwner()
}
