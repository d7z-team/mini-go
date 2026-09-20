package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestSchedulerRotatesRunnableTasks(t *testing.T) {
	for _, budgets := range [][]int{{1}, {taskInstructionQuantum - 1}, {taskInstructionQuantum}, {taskInstructionQuantum + 1}, {1, 7, 31}, {65536}} {
		t.Run(fmt.Sprint(budgets), func(t *testing.T) { testSchedulerRotation(t, budgets) })
	}
}

func TestTaskSliceReturnsAtQuantumWithoutReadyPeers(t *testing.T) {
	artifact := ir.NewArtifact("scheduler/bounded-slice", "main")
	var instructions []ir.Instruction
	for range taskInstructionQuantum * 2 {
		instructions = append(instructions,
			ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			ir.Instruction{Op: string(ir.OpPop)},
		)
	}
	instructions = append(instructions, ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})})
	artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function()"), Instructions: instructions}}
	instance, err := patchTestProgram(t, artifact, "bounded-slice").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Start("run"); err != nil {
		t.Fatal(err)
	}
	machine := instance.vm.machine
	task := machine.popRunnable()
	if !task.ownership.acquire() {
		t.Fatal("task did not acquire lease")
	}
	slice := taskSlice{limit: taskInstructionQuantum * 8}
	yield, _, err := machine.runTask(task, &slice)
	task.ownership.relinquish()
	machine.abortTask(task)
	machine.finishTask(task, nil)
	if err != nil || yield.kind != taskYieldCooperate || slice.executed != taskInstructionQuantum {
		t.Fatalf("unbounded worker slice: yield=%s executed=%d err=%v", yield.kind, slice.executed, err)
	}
}

func testSchedulerRotation(t *testing.T, budgets []int) {
	t.Helper()
	artifact := ir.NewArtifact("scheduler/fairness", "main")
	artifact.Constants = []ir.Constant{{ID: "const.true", Type: testType("Bool"), Value: json.RawMessage(`true`)}}
	artifact.Globals = []ir.Global{
		{ID: "global.ready", Type: testType("Bool")},
		{ID: "global.observed", Type: testType("Bool")},
	}
	child := make([]ir.Instruction, 0, taskInstructionQuantum*2+4)
	for range taskInstructionQuantum * 2 {
		child = append(child,
			ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			ir.Instruction{Op: string(ir.OpPop)},
		)
	}
	child = append(child,
		ir.Instruction{Op: string(ir.OpLoadGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.ready"})},
		ir.Instruction{Op: string(ir.OpStoreGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.observed"})},
		ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	)
	artifact.Functions = []ir.Function{{
		ID: "fn.main", Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.child"})},
			{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
			{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.true"})},
			{Op: string(ir.OpStoreGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.ready"})},
		},
	}, {
		ID: "fn.child", RevisionLocal: true,
		Signature: testSignature("function() Void"), Instructions: child,
	}}
	for range taskInstructionQuantum * 2 {
		artifact.Functions[0].Instructions = append(artifact.Functions[0].Instructions,
			ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			ir.Instruction{Op: string(ir.OpPop)},
		)
	}
	artifact.Functions[0].Instructions = append(artifact.Functions[0].Instructions,
		ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	)
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	vm := pollSchedulerArtifact(t, artifact, budgets)
	observed := vm.rootModule().state.globals["global.observed"].load()
	if value, ok := observed.Data.(bool); !ok || !value {
		t.Fatalf("child observed ready = %#v", observed)
	}
}

func pollSchedulerArtifact(t *testing.T, artifact ir.Artifact, budgets []int) *vm {
	t.Helper()
	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	instance := &Instance{vm: vm, done: make(chan struct{})}
	t.Cleanup(vm.closeRevisions)
	execution, err := instance.start(context.Background(), false, func(*instanceRevision) (int64, error) {
		return vm.prepareFunction("fn.main", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	for poll := 0; execution.State() == ExecutionRunning; poll++ {
		if poll > taskInstructionQuantum*20 {
			t.Fatal("execution did not complete")
		}
		if _, _, err := execution.PollSteps(budgets[poll%len(budgets)]); err != nil {
			t.Fatal(err)
		}
	}
	return vm
}
