package runtime

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func BenchmarkVMIntegerLoop(b *testing.B) {
	machine, err := loadTestEngine(slotLoopArtifact(b, 1000))
	if err != nil {
		b.Fatal(err)
	}
	before := machine.executedSteps
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := runTestModuleExport(machine, "Main"); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(machine.executedSteps-before)/float64(b.N), "steps/op")
}

func BenchmarkVMSchedulerRotation(b *testing.B) {
	const workPairs = 2048
	artifact := ir.NewArtifact("benchmark/scheduler", "main")
	artifact.Globals = []ir.Global{{ID: "global.done", Type: testType("Bool")}}
	child := testSlotCode([]string{"Bool", "Bool"}, []ir.Instruction{
		{Op: ir.OpZero, Payload: testTypePayload("Bool")},
		{Op: ir.OpUnary, Payload: ir.OperatorPayload{Operator: "!"}},
		{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.done"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
	}, [][2][]uint32{{nil, {0}}, {{0}, {1}}, {{1}, nil}, {nil, nil}})
	insertTestDelay(child, 0, workPairs)
	main := testSlotCode([]string{"Bool", "function() Void"}, []ir.Instruction{
		{Op: ir.OpZero, Payload: testTypePayload("Bool")},
		{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.done"}},
		{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "fn.child"}},
		{Op: ir.OpSpawn, Payload: ir.CallPayload{}},
	}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}})
	insertTestDelay(main, len(main.Instructions), workPairs)
	appendTestSlotCode(main, []ir.Instruction{
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "wait"}},
		{Op: ir.OpLoadGlobal, Payload: ir.GlobalPayload{Global: "global.done"}},
		{Op: ir.OpJumpIf, Payload: ir.JumpPayload{Label: "done"}},
		{Op: ir.OpJump, Payload: ir.JumpPayload{Label: "wait"}},
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "done"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
	}, [][2][]uint32{{nil, nil}, {nil, {0}}, {{0}, nil}, {nil, nil}, {nil, nil}, {nil, nil}})
	artifact.Functions = []ir.Function{
		{ID: "fn.main", Signature: testSignature("function() Void"), Code: main},
		{ID: "fn.child", RevisionLocal: true, Signature: testSignature("function() Void"), Code: child},
	}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	machine, err := loadTestEngine(artifact)
	if err != nil {
		b.Fatal(err)
	}
	before := machine.executedSteps
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := runTestModuleExport(machine, "Main"); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(machine.executedSteps-before)/float64(b.N), "steps/op")
}
