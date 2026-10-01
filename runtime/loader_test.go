package runtime

import (
	"encoding/json"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLoaderValidatesSlotDestinationsForStructuredAndJSONInputs(t *testing.T) {
	artifact := ir.NewArtifact("test/slots", "slots")
	artifact.Functions = []ir.Function{{
		ID: "fn.value", Signature: testSignature("function() Bool"),
		Code: testSlotCode([]string{"Bool"}, []ir.Instruction{
			{Op: ir.OpZero, Payload: testTypePayload("Bool")},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	attachRuntimeTestTypeNodes(&artifact)
	for _, destination := range []uint32{0, 1, 0} {
		artifact.Functions[0].Code.Operands[0].Outputs[0] = destination
		data, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal(err)
		}
		for _, jsonInput := range []bool{false, true} {
			var loaded *executable
			if jsonInput {
				loaded, err = newLoader().loadJSON(data)
			} else {
				loaded, err = newLoader().load(artifact)
			}
			if destination == 1 {
				if err == nil {
					t.Fatal("out-of-range destination accepted")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if fn := loaded.Functions["fn.value"]; len(fn.Decl.Code.Types) != 1 || fn.Instructions[0].operands.Outputs[0] != 0 {
				t.Fatal("prepared function lost its destination layout")
			}
		}
	}
}

func TestLoaderRemovesLabelsAndRelocatesJumps(t *testing.T) {
	artifact := ir.NewArtifact("test/labels", "labels")
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{}, []ir.Instruction{
			{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "entry"}},
			{Op: ir.OpJump, Payload: ir.JumpPayload{Label: "exit"}},
			{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "exit"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
		}, [][2][]uint32{{nil, nil}, {nil, nil}, {nil, nil}, {nil, nil}}),
	}}
	attachRuntimeTestTypeNodes(&artifact)

	executable, err := newLoader().load(artifact)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	function := executable.Functions["fn.main"]
	if len(function.Instructions) != 2 {
		t.Fatalf("prepared instruction count = %d, want 2", len(function.Instructions))
	}
	if function.Instructions[0].op != preparedJump || function.Instructions[0].jumpPC != 1 {
		t.Fatalf("prepared jump = %#v, want target pc 1", function.Instructions[0])
	}
}
