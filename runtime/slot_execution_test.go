package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestSlotLoopRespectsPollAndInstanceBudgets(t *testing.T) {
	const iterations, work = 9, 28
	program := patchTestProgram(t, slotLoopArtifact(t, iterations), "slot-loop")
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
	inst := preparedInstruction{operands: &ir.SlotOperands{ReleaseBefore: []uint32{0}}}
	f.beginSlotInstruction(1, &inst)
	if f.slotValues[0] != (vmValue{}) {
		t.Fatal("block entry retained expired reference")
	}
	if f.slotValues[1] != newVMValue("String", "live") {
		t.Fatal("block entry cleared live reference")
	}
	f.slotValues[0] = newVMValue("String", "pending result")
	f.beginSlotInstruction(1, &inst)
	if f.slotValues[0] != newVMValue("String", "pending result") {
		t.Fatal("retry repeated entry release")
	}
}

func TestDirectLocalOutputLoopCommitsWithinOneStep(t *testing.T) {
	artifact := slotLoopArtifact(t, 9)
	code := artifact.Functions[0].Code
	code.Operands[1].Outputs[0] = ir.LocalOutput
	code.Instructions = append(code.Instructions[:2], code.Instructions[3:]...)
	// The eliminated store's temporary is never produced or observed.
	code.Types = nil
	code.Operands[2] = ir.SlotOperands{}
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

func TestDirectPointerInputsPreservePointeeAndLazyNil(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	pointer := types.TypeRef{Kind: types.Pointer, Node: "pointer"}
	for _, nilResult := range []bool{false, true} {
		artifact := ir.NewArtifact("slot/pointer", "main")
		artifact.TypeTable.Nodes = []types.TypeNode{{ID: "pointer", Kind: types.Pointer, Elem: integer}}
		artifact.Constants = []ir.Constant{{ID: "initial", Type: integer, Value: json.RawMessage(`41`)}, {ID: "updated", Type: integer, Value: json.RawMessage(`42`)}}
		code := ir.SlotCode{Types: []types.TypeRef{pointer, integer}}
		instructions := []ir.Instruction{
			{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "value"}},
			{Op: ir.OpAddressOf, Payload: ir.AddressPayload{Kind: "local", Local: "value"}},
			{Op: ir.OpStoreLocal, Payload: ir.LocalPayload{Local: "pointer"}},
			{Op: ir.OpStoreIndirect},
			{Op: ir.OpLoadIndirect},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}
		code.Operands = []ir.SlotOperands{
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
			code.Operands = []ir.SlotOperands{{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 1}}}}
		}
		for pc, instruction := range instructions {
			descriptor, err := code.Descriptors.Append(instruction.Payload)
			if err != nil {
				t.Fatal(err)
			}
			code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc)})
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

func TestSlotExecutionRetainsCallDestinationsAcrossPollBudgets(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	artifact := ir.NewArtifact("slot/calls", "main")
	artifact.Constants = []ir.Constant{{ID: "forty", Type: integer, Value: json.RawMessage("40")}, {ID: "two", Type: integer, Value: json.RawMessage("2")}}
	main := ir.SlotCode{Types: []types.TypeRef{integer, integer}}
	child := ir.SlotCode{Types: []types.TypeRef{integer}}
	emit := func(code *ir.SlotCode, op ir.Opcode, payload ir.Payload, inputs []ir.Operand, outputs, release []uint32) {
		index, err := code.Descriptors.Append(payload)
		if err != nil {
			t.Fatal(err)
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: op, Descriptor: index, Operands: uint32(len(code.Operands))})
		code.Operands = append(code.Operands, ir.SlotOperands{Inputs: inputs, Outputs: outputs, Release: release})
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
