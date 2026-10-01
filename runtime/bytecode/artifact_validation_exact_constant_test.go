package bytecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestArtifactValidationAcceptsExactUntypedNumericWire(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{
		{ID: "const.Third", Type: testType("Float64"), Value: json.RawMessage(`"1/3"`), Untyped: true},
		{ID: "const.Value", Type: testType("Complex128"), Value: json.RawMessage(`{"real":"3/2","imag":"-5/4"}`), Untyped: true},
	}
	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
}

func TestArtifactValidationRejectsMalformedExactUntypedNumericWire(t *testing.T) {
	for _, test := range []struct {
		name  string
		typ   string
		value string
	}{
		{name: "runtime float wire", typ: "Float64", value: `0.5`},
		{name: "missing denominator", typ: "Float64", value: `"1"`},
		{name: "zero denominator", typ: "Float64", value: `"1/0"`},
		{name: "noncanonical zero", typ: "Float64", value: `"0/2"`},
		{name: "numeric complex component", typ: "Complex128", value: `{"real":1,"imag":"0/1"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := NewArtifact("example/module", "main")
			artifact.Constants = []Constant{{ID: "const.Bad", Type: testType(test.typ), Value: json.RawMessage(test.value), Untyped: true}}
			err := testValidateArtifact(&artifact)
			if err == nil || !strings.Contains(err.Error(), "untyped") {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}
}

func TestArtifactValidationRejectsUntypedConstantInRuntimeInstruction(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{{
		ID:      "const.Third",
		Type:    testType("Float64"),
		Value:   json.RawMessage(`"1/3"`),
		Untyped: true,
	}}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Float64"}, []Instruction{{
			Op:      OpConst,
			Payload: ConstPayload{Constant: "const.Third"},
		}}, [][2][]uint32{{nil, {0}}}),
	}}
	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "cannot be used by runtime instructions") {
		t.Fatalf("Validate error = %v", err)
	}
}
