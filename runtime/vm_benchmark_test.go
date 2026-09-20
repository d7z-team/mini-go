package runtime

import (
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func BenchmarkVMIntegerLoop(b *testing.B) {
	artifact := ir.NewArtifact("benchmark/loop", "main")
	artifact.Constants = []ir.Constant{
		{ID: "const.iterations", Type: testType("Int64"), Value: json.RawMessage(`1000`)},
		{ID: "const.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)},
		{ID: "const.one", Type: testType("Int64"), Value: json.RawMessage(`1`)},
	}
	artifact.Functions = []ir.Function{{
		ID: "fn.main", Signature: testSignature("function() Int64"),
		Locals: []ir.Local{{ID: "local.remaining", Type: testType("Int64")}},
		Instructions: []ir.Instruction{
			{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.iterations"})},
			{Op: string(ir.OpStoreLocal), Payload: testPayload(ir.LocalPayload{Local: "local.remaining"})},
			{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "loop"})},
			{Op: string(ir.OpLoadLocal), Payload: testPayload(ir.LocalPayload{Local: "local.remaining"})},
			{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.zero"})},
			{Op: string(ir.OpBinary), Payload: testPayload(ir.OperatorPayload{Operator: "=="})},
			{Op: string(ir.OpJumpIf), Payload: testPayload(ir.JumpPayload{Label: "done"})},
			{Op: string(ir.OpLoadLocal), Payload: testPayload(ir.LocalPayload{Local: "local.remaining"})},
			{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.one"})},
			{Op: string(ir.OpBinary), Payload: testPayload(ir.OperatorPayload{Operator: "-"})},
			{Op: string(ir.OpStoreLocal), Payload: testPayload(ir.LocalPayload{Local: "local.remaining"})},
			{Op: string(ir.OpJump), Payload: testPayload(ir.JumpPayload{Label: "loop"})},
			{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "done"})},
			{Op: string(ir.OpLoadLocal), Payload: testPayload(ir.LocalPayload{Local: "local.remaining"})},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{ResultCount: 1})},
		},
	}}
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

func BenchmarkVMSchedulerRotation(b *testing.B) {
	const workPairs = 2048
	artifact := ir.NewArtifact("benchmark/scheduler", "main")
	artifact.Globals = []ir.Global{{ID: "global.done", Type: testType("Bool")}}
	work := make([]ir.Instruction, 0, workPairs*2+3)
	for range workPairs {
		work = append(work,
			ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
			ir.Instruction{Op: string(ir.OpPop)},
		)
	}
	child := append([]ir.Instruction(nil), work...)
	child = append(child,
		ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
		ir.Instruction{Op: string(ir.OpUnary), Payload: testPayload(ir.OperatorPayload{Operator: "!"})},
		ir.Instruction{Op: string(ir.OpStoreGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.done"})},
		ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	)
	main := []ir.Instruction{
		{Op: string(ir.OpZero), Payload: testTypePayload("Bool")},
		{Op: string(ir.OpStoreGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.done"})},
		{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.child"})},
		{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
	}
	main = append(main, work...)
	main = append(main,
		ir.Instruction{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "wait"})},
		ir.Instruction{Op: string(ir.OpLoadGlobal), Payload: testPayload(ir.GlobalPayload{Global: "global.done"})},
		ir.Instruction{Op: string(ir.OpJumpIf), Payload: testPayload(ir.JumpPayload{Label: "done"})},
		ir.Instruction{Op: string(ir.OpJump), Payload: testPayload(ir.JumpPayload{Label: "wait"})},
		ir.Instruction{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "done"})},
		ir.Instruction{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	)
	artifact.Functions = []ir.Function{
		{ID: "fn.main", Signature: testSignature("function() Void"), Instructions: main},
		{ID: "fn.child", RevisionLocal: true, Signature: testSignature("function() Void"), Instructions: child},
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
