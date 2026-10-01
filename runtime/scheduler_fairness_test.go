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
	code := testSlotCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}})
	insertTestDelay(code, 0, taskInstructionQuantum*2)
	artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function()"), Code: code}}
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
	child := testSlotCode([]string{"Bool"}, []ir.Instruction{
		{Op: ir.OpLoadGlobal, Payload: ir.GlobalPayload{Global: "global.ready"}},
		{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.observed"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
	}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}})
	insertTestDelay(child, 0, taskInstructionQuantum*2)
	artifact.Functions = []ir.Function{{
		ID: "fn.main", Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void", "Bool"}, []ir.Instruction{
			{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "fn.child"}},
			{Op: ir.OpSpawn, Payload: ir.CallPayload{}},
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.true"}},
			{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.ready"}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}, {
		ID: "fn.child", RevisionLocal: true,
		Signature: testSignature("function() Void"), Code: child,
	}}
	insertTestDelay(artifact.Functions[0].Code, 4, taskInstructionQuantum*2)
	appendTestSlotCode(artifact.Functions[0].Code, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}})
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
