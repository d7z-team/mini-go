package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestMicrocodeLoopRespectsPollAndInstanceBudgets(t *testing.T) {
	const iterations, work = 9, 28
	program := patchTestProgram(t, microcodeLoopArtifact(t, iterations), "microcode-loop")
	for _, limit := range []int64{UnlimitedSteps, 1, 3, 8, 15, work - 1, work, work + 1} {
		for quantum := 1; quantum <= 12; quantum++ {
			t.Run(fmt.Sprintf("limit=%d/quantum=%d", limit, quantum), func(t *testing.T) {
				instance, err := program.Instantiate(t.Context(), InstanceOptions{Limits: Limits{MaxSteps: limit}})
				if err != nil {
					t.Fatal(err)
				}
				cleanupTestInstance(t, instance)
				execution, err := instance.Start("run")
				if err != nil {
					t.Fatal(err)
				}
				total := 0
				for {
					state, steps, pollErr := execution.PollSteps(quantum)
					if steps < 0 || steps > quantum {
						t.Fatalf("poll executed %d of %d", steps, quantum)
					}
					total += steps
					if pollErr != nil {
						if limit < 0 || limit >= work || int64(total) != limit || !strings.Contains(pollErr.Error(), "step") {
							t.Fatalf("unexpected failure after %d steps: %v", total, pollErr)
						}
						break
					}
					if state == ExecutionCompleted {
						result, err := execution.resultValues()
						if err != nil {
							t.Fatal(err)
						}
						requireValues(t, result.Values, newVMValue("Int", int64(iterations)))
						if total != work {
							t.Fatalf("charged work %d, want %d", total, work)
						}
						break
					}
					if total > work {
						t.Fatal("loop failed to terminate")
					}
				}
			})
		}
	}
}

func TestSlotEntryReleasePreservesRetryInputs(t *testing.T) {
	f := frame{slotPC: -1, slotValues: []vmValue{newVMValue("String", "expired"), newVMValue("String", "live")}}
	inst := preparedInstruction{operands: &ir.RegisterFlow{ReleaseBefore: []uint32{0}}}
	f.beginMicroInstruction(1, &inst)
	if f.slotValues[0] != (vmValue{}) {
		t.Fatal("block entry retained expired reference")
	}
	if f.slotValues[1] != newVMValue("String", "live") {
		t.Fatal("block entry cleared live reference")
	}
	f.slotValues[0] = newVMValue("String", "pending result")
	f.beginMicroInstruction(1, &inst)
	if f.slotValues[0] != newVMValue("String", "pending result") {
		t.Fatal("retry repeated entry release")
	}
}

func TestDirectLocalOutputLoopCommitsWithinOneStep(t *testing.T) {
	artifact := microcodeLoopArtifact(t, 9)
	code := artifact.Functions[0].Code
	code.Operands[1].Outputs[0] = ir.LocalOutput
	code.Instructions = append(code.Instructions[:2], code.Instructions[3:]...)
	// The eliminated store's temporary is never produced or observed.
	code.Types = nil
	code.Operands[2] = ir.RegisterFlow{}
	code.Blocks = []ir.BasicBlock{
		{First: 0, Count: 2, Fallthrough: 1, Cost: 2},
		{First: 2, Count: 1, Fallthrough: ir.NoBlock, Cost: 1},
	}
	program := patchTestProgram(t, artifact, "direct-local-loop")
	instance, err := program.Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	steps := 0
	for {
		state, count, err := execution.PollSteps(1)
		if err != nil {
			t.Fatal(err)
		}
		if count < 0 || count > 1 {
			t.Fatalf("one-step poll executed %d", count)
		}
		steps += count
		if state == ExecutionCompleted {
			break
		}
		if steps > 19 {
			t.Fatal("loop failed to terminate")
		}
	}
	result, err := execution.resultValues()
	if err != nil {
		t.Fatal(err)
	}
	requireValues(t, result.Values, newVMValue("Int", int64(9)))
	if steps != 19 {
		t.Fatalf("charged %d steps, want 19", steps)
	}
}

func TestUpdateCompareBranchChargesBeforeCommittingTheUpdate(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	artifact := ir.NewArtifact("microcode/update-branch", "main")
	artifact.Constants = []ir.Constant{
		{ID: "one", Type: integer, Value: json.RawMessage(`1`)},
		{ID: "end", Type: integer, Value: json.RawMessage(`3`)},
	}
	code := ir.MicroCode{Operands: []ir.RegisterFlow{
		{},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant}, {Kind: ir.OperandConstant, Index: 1}}},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}}},
	}}
	label, err := code.Descriptors.Append(ir.LabelPayload{Label: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	update, err := code.Descriptors.Append(ir.UpdateCompareBranchPayload{
		Update: "+", Comparison: "<", Type: integer, Label: "loop", When: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ret, err := code.Descriptors.Append(ir.ReturnPayload{ResultCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	mode := uint16(types.PrimitiveInt)
	code.Instructions = []ir.MicroInstruction{
		{Op: ir.OpLabel, Descriptor: label, Operands: 0, Cost: 1},
		{Op: ir.OpUpdateCompareBranch, Mode: mode, Descriptor: update, Operands: 1, Cost: 2},
		{Op: ir.OpReturn, Mode: mode, Descriptor: ret, Operands: 2, Cost: 1},
	}
	code.Blocks = []ir.BasicBlock{
		{First: 0, Count: 1, Fallthrough: 1, Cost: 2},
		{First: 1, Count: 1, Fallthrough: ir.NoBlock, Cost: 1},
	}
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: types.FunctionSignature{Results: []types.TypeRef{integer}},
		Locals: []ir.Local{{ID: "counter", Type: integer}}, Code: &code,
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.entry"}}
	instance, err := patchTestProgram(t, artifact, "update-branch").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	for step := 1; step <= 7; step++ {
		state, executed, pollErr := execution.PollSteps(1)
		if pollErr != nil || executed != 1 {
			t.Fatalf("logical step %d = %s, %d, %v", step, state, executed, pollErr)
		}
		if step < 7 && state != ExecutionRunning {
			t.Fatalf("logical step %d settled early: %s", step, state)
		}
		if step == 7 && state != ExecutionCompleted {
			t.Fatalf("final logical step = %s", state)
		}
	}
	result, err := execution.resultValues()
	if err != nil {
		t.Fatal(err)
	}
	requireValues(t, result.Values, newVMValue("Int", int64(3)))
}

func TestFastScalarRunResumesAtCurrentInstructionAfterPartialPoll(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	artifact := ir.NewArtifact("microcode/fast-resume", "main")
	artifact.Constants = []ir.Constant{
		{ID: "one", Type: integer, Value: json.RawMessage(`1`)},
		{ID: "end", Type: integer, Value: json.RawMessage(`3`)},
	}
	code := ir.MicroCode{Operands: []ir.RegisterFlow{
		{},
		{Outputs: []uint32{ir.LocalOutput | 1}},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant}, {Kind: ir.OperandConstant, Index: 1}}},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}}},
	}}
	label, err := code.Descriptors.Append(ir.LabelPayload{Label: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	zero, err := code.Descriptors.Append(ir.TypePayload{Type: integer})
	if err != nil {
		t.Fatal(err)
	}
	update, err := code.Descriptors.Append(ir.UpdateCompareBranchPayload{
		Update: "+", Comparison: "<", Type: integer, Label: "loop", When: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ret, err := code.Descriptors.Append(ir.ReturnPayload{ResultCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	mode := uint16(types.PrimitiveInt)
	code.Instructions = []ir.MicroInstruction{
		{Op: ir.OpLabel, Descriptor: label, Operands: 0, Cost: 1},
		{Op: ir.OpZero, Mode: mode, Descriptor: zero, Operands: 1, Cost: 1},
		{Op: ir.OpUpdateCompareBranch, Mode: mode, Descriptor: update, Operands: 2, Cost: 3},
		{Op: ir.OpReturn, Mode: mode, Descriptor: ret, Operands: 3, Cost: 1},
	}
	code.Blocks = []ir.BasicBlock{
		{First: 0, Count: 2, Fallthrough: 1, Cost: 4},
		{First: 2, Count: 1, Fallthrough: ir.NoBlock, Cost: 1},
	}
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: types.FunctionSignature{Results: []types.TypeRef{integer}},
		Locals: []ir.Local{{ID: "counter", Type: integer}, {ID: "scratch", Type: integer}}, Code: &code,
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.entry"}}
	instance, err := patchTestProgram(t, artifact, "fast-resume").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for {
		state, executed, pollErr := execution.PollSteps(2)
		if pollErr != nil {
			t.Fatal(pollErr)
		}
		total += executed
		if state == ExecutionCompleted {
			break
		}
		if total > 13 {
			t.Fatal("loop failed to terminate")
		}
	}
	result, err := execution.resultValues()
	if err != nil {
		t.Fatal(err)
	}
	requireValues(t, result.Values, newVMValue("Int", int64(3)))
	if total != 13 {
		t.Fatalf("charged %d logical steps, want 13", total)
	}
}

func TestDirectPointerInputsPreservePointeeAndLazyNil(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	pointer := types.TypeRef{Kind: types.Pointer, Node: "pointer"}
	for _, nilResult := range []bool{false, true} {
		artifact := ir.NewArtifact("slot/pointer", "main")
		artifact.TypeTable.Nodes = []types.TypeNode{{ID: "pointer", Kind: types.Pointer, Elem: integer}}
		artifact.Constants = []ir.Constant{{ID: "initial", Type: integer, Value: json.RawMessage(`41`)}, {ID: "updated", Type: integer, Value: json.RawMessage(`42`)}}
		code := ir.MicroCode{Types: []types.TypeRef{pointer, integer}}
		instructions := []ir.Instruction{
			{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "value"}},
			{Op: ir.OpAddressOf, Payload: ir.AddressPayload{Kind: "local", Local: "value"}},
			{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "pointer"}},
			{Op: ir.OpStoreIndirect},
			{Op: ir.OpLoadIndirect},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}
		code.Operands = []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandConstant}}},
			{Outputs: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 1}, {Kind: ir.OperandConstant, Index: 1}}},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 1}}, Outputs: []uint32{1}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}, Release: []uint32{1}},
		}
		resultType := integer
		if nilResult {
			resultType = pointer
			instructions = instructions[len(instructions)-1:]
			code.Operands = []ir.RegisterFlow{{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 1}}}}
		}
		for pc, instruction := range instructions {
			descriptor, err := code.Descriptors.Append(instruction.Payload)
			if err != nil {
				t.Fatal(err)
			}
			code.Instructions = append(code.Instructions, ir.MicroInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc)})
		}
		artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: types.FunctionSignature{Results: []types.TypeRef{resultType}}, Locals: []ir.Local{{ID: "value", Type: integer}, {ID: "pointer", Type: pointer}}, Code: &code}}
		program := patchTestProgram(t, artifact, "slot-pointer")
		for _, quantum := range []int{1, 2, 8} {
			instance, err := program.Instantiate(t.Context(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestInstance(t, instance)
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			total := 0
			for {
				state, steps, err := execution.PollSteps(quantum)
				if err != nil {
					t.Fatal(err)
				}
				total += steps
				if steps > quantum || total > len(instructions) {
					t.Fatalf("unexpected budget: %d/%d", steps, total)
				}
				if state == ExecutionCompleted {
					break
				}
			}
			result, err := execution.resultValues()
			if err != nil {
				t.Fatal(err)
			}
			if total != len(instructions) {
				t.Fatalf("charged %d steps, want %d", total, len(instructions))
			}
			if nilResult {
				if len(result.Values) != 1 || result.Values[0].Data != nil {
					t.Fatalf("pointer zero: %+v", result.Values)
				}
			} else {
				requireValues(t, result.Values, newVMValue("Int", int64(42)))
			}
		}
	}
}

func TestMicrocodeExecutionRetainsCallDestinationsAcrossPollBudgets(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	artifact := ir.NewArtifact("slot/calls", "main")
	artifact.Constants = []ir.Constant{{ID: "forty", Type: integer, Value: json.RawMessage("40")}, {ID: "two", Type: integer, Value: json.RawMessage("2")}}
	main := ir.MicroCode{Types: []types.TypeRef{integer, integer}}
	child := ir.MicroCode{Types: []types.TypeRef{integer}}
	emit := func(code *ir.MicroCode, op ir.Opcode, payload ir.Payload, inputs []ir.Operand, outputs, release []uint32) {
		index, err := code.Descriptors.Append(payload)
		if err != nil {
			t.Fatal(err)
		}
		code.Instructions = append(code.Instructions, ir.MicroInstruction{Op: op, Descriptor: index, Operands: uint32(len(code.Operands))})
		code.Operands = append(code.Operands, ir.RegisterFlow{Inputs: inputs, Outputs: outputs, Release: release})
	}
	emit(&main, ir.OpCallDirect, ir.CallPayload{Function: "fn.child", ArgCount: 1, ResultCount: 1}, []ir.Operand{{Kind: ir.OperandConstant, Index: 1}}, []uint32{0}, nil)
	emit(&main, ir.OpBinary, ir.OperatorPayload{Operator: "+"}, []ir.Operand{{Kind: ir.OperandConstant, Index: 0}, {Kind: ir.OperandSlot, Index: 0}}, []uint32{1}, []uint32{0})
	emit(&main, ir.OpReturn, ir.ReturnPayload{ResultCount: 1}, []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}, nil, []uint32{1})
	emit(&child, ir.OpLoadLocal, ir.LocalPayload{Local: "argument"}, nil, []uint32{0}, nil)
	emit(&child, ir.OpReturn, ir.ReturnPayload{ResultCount: 1}, []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}, nil, []uint32{0})
	artifact.Functions = []ir.Function{
		{ID: "fn.entry", Signature: types.FunctionSignature{Results: []types.TypeRef{integer}}, Code: &main},
		{ID: "fn.child", Signature: types.FunctionSignature{Params: []types.TypeParam{{Type: integer}}, Results: []types.TypeRef{integer}}, Locals: []ir.Local{{ID: "argument", Type: integer}}, Code: &child},
	}
	for _, quantum := range []int{1, 2, 64} {
		instance, err := patchTestProgram(t, artifact, "slot-calls").Instantiate(t.Context(), InstanceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		execution, err := instance.Start("run")
		if err != nil {
			t.Fatal(err)
		}
		steps := 0
		for {
			state, executed, err := execution.PollSteps(quantum)
			if err != nil {
				t.Fatal(err)
			}
			if executed > quantum {
				t.Fatalf("executed %d, budget %d", executed, quantum)
			}
			steps += executed
			if state == ExecutionCompleted {
				break
			}
			if steps > 10 {
				t.Fatal("call failed to complete")
			}
		}
		result, err := execution.resultValues()
		if err != nil {
			t.Fatal(err)
		}
		requireValues(t, result.Values, newVMValue("Int", int64(42)))
		if steps != 5 {
			t.Fatalf("charged work = %d, want 5", steps)
		}
		if err := instance.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
