package bytecode

import "testing"

func TestArtifactLimitsIdentifyIndividualAndAggregateExcess(t *testing.T) {
	artifact := Artifact{
		Functions: []Function{{Code: &SlotCode{}}, {
			Locals: make([]Local, 2), Upvalues: make([]Upvalue, 2),
			Code: &SlotCode{Instructions: []SlotInstruction{{Op: OpReturn}, {Op: OpCallDirect}}, Descriptors: DescriptorTables{Return: []ReturnPayload{{}}, Call: []CallPayload{{}}}},
		}},
		Constants: []Constant{{Value: []byte(`0`)}, {Value: []byte(`123`)}},
	}
	for _, test := range []struct {
		limits ValidationLimits
		path   string
	}{
		{ValidationLimits{MaxLocalsPerFunction: 1}, "functions[1].locals"},
		{ValidationLimits{MaxUpvaluesPerFunction: 1}, "functions[1].upvalues"},
		{ValidationLimits{MaxInstructions: 1}, "instructions"},
		{ValidationLimits{MaxPayloadBytes: DescriptorBytes(CallPayload{})}, "instruction_payload_bytes"},
		{ValidationLimits{MaxConstantBytes: 2}, "constants[1].value_bytes"},
		{ValidationLimits{MaxConstantBytes: 3}, "constant_value_bytes"},
	} {
		t.Run(test.path, func(t *testing.T) {
			issue, ok := ValidationIssueFromError(validateArtifactLimits(&artifact, test.limits))
			if !ok || issue.Code != ValidationLimitExceeded || issue.Path != test.path {
				t.Fatalf("got %+v; want limit exceeded at %s", issue, test.path)
			}
		})
	}
	for _, limits := range []ValidationLimits{{}, {MaxLocalsPerFunction: 2, MaxUpvaluesPerFunction: 2, MaxInstructions: 2, MaxPayloadBytes: DescriptorBytes(ReturnPayload{}) + DescriptorBytes(CallPayload{}) + 24, MaxConstantBytes: 4}} {
		if err := validateArtifactLimits(&artifact, limits); err != nil {
			t.Fatal(err)
		}
	}
}
