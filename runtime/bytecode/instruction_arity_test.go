package bytecode

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestInstructionOperandArity(t *testing.T) {
	for _, tc := range []struct {
		instruction     Instruction
		inputs, outputs int
	}{
		{Instruction{Op: OpLabel, Payload: LabelPayload{Label: "next"}}, 0, 0},
		{Instruction{Op: OpBinary, Payload: OperatorPayload{Operator: "+"}}, 2, 1},
		{Instruction{Op: OpMapIterNext, Payload: LocalPayload{Local: "iter"}}, 0, 3},
		{Instruction{Op: OpMakeMap, Payload: MakeMapPayload{EntryCount: 3, HasCapacity: true}}, 7, 1},
		{Instruction{Op: OpMakeSlice, Payload: MakeSlicePayload{HasCapacity: true}}, 2, 1},
		{Instruction{Op: OpMakeSlice, Payload: MakeSlicePayload{}}, 1, 1},
		{Instruction{Op: OpCallValue, Payload: CallPayload{ArgCount: 3, ResultCount: 2}}, 4, 2},
		{Instruction{Op: OpTailCallDirect, Payload: CallPayload{ArgCount: 3, ResultCount: 2}}, 3, 0},
		{Instruction{Op: OpCallFFI, Payload: CallFFIPayload{ArgCount: 2, ResultCount: 3}}, 2, 3},
	} {
		inputs, outputs, err := instructionArity(&tc.instruction)
		if err != nil || inputs != tc.inputs || outputs != tc.outputs {
			t.Fatalf("%s: (%d,%d), %v", tc.instruction.Op, inputs, outputs, err)
		}
	}
	for _, instruction := range []Instruction{{Op: Opcode(65535)}, {Op: OpCallDirect, Payload: LabelPayload{}}, {Op: OpMakeMap}} {
		if _, _, err := instructionArity(&instruction); err == nil {
			t.Fatalf("accepted invalid instruction: %+v", instruction)
		}
	}
}

func TestOpcodeArityContractCoversPayloadRules(t *testing.T) {
	for _, spec := range OpcodeSpecs() {
		t.Run(spec.Op, func(t *testing.T) {
			op, ok := ParseOpcode(spec.Op)
			if !ok {
				t.Fatal("unknown operation")
			}
			instruction := Instruction{Op: op}
			switch spec.Arity.Kind {
			case "fixed":
				if spec.Arity.Inputs < 0 || spec.Arity.Outputs < 0 {
					t.Fatal("negative arity")
				}
			case "payload":
				if spec.Payload == "" || spec.Arity.Inputs != 0 || spec.Arity.Outputs != 0 {
					t.Fatal("invalid dynamic arity")
				}
				if err := json.Unmarshal([]byte(fmt.Sprintf(`{"op":%q,"payload":{}}`, spec.Op)), &instruction); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("missing arity classification")
			}
			if _, _, err := instructionArity(&instruction); err != nil {
				t.Fatal(err)
			}
		})
	}
}
