package bytecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateArtifactRejectsUnknownExportRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []Instruction{{
			Op:      string(OpLoadExport),
			Payload: json.RawMessage(`{"module_path":"example/lib","export":"Missing"}`),
		}, {
			Op: string(OpPop),
		}},
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown module requirement") {
		t.Fatalf("expected unknown module requirement error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownInitModuleRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []Instruction{{
			Op:      string(OpInitModule),
			Payload: json.RawMessage(`{"module_path":"example/lib"}`),
		}},
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown module requirement") {
		t.Fatalf("expected unknown module requirement error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownExportTarget(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Exports = []Export{{Name: "Main", Kind: "function", ID: "fn.missing"}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown function id") {
		t.Fatalf("expected unknown function id error, got %v", err)
	}
}

func TestValidateArtifactRejectsDuplicateExportNames(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Exports = []Export{
		{Name: "Value", Kind: "type", ID: "type.First", Type: testType("Int64")},
		{Name: "Value", Kind: "type", ID: "type.Second", Type: testType("String")},
	}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), `duplicate export "Value"`) {
		t.Fatalf("expected duplicate export error, got %v", err)
	}
}

func TestValidateArtifactAcceptsConstExportTarget(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{{ID: "const.Answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Exports = []Export{{Name: "Answer", Kind: "const", ID: "const.Answer", Type: testType("Int64")}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactAcceptsTypeAliasExport(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Exports = []Export{{Name: "Alias", Kind: "type", ID: "type.Alias", Type: testType("Int64")}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactRejectsTypeAliasExportWithoutType(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Exports = []Export{{Name: "Alias", Kind: "type", ID: "type.Alias"}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), `unknown type id "type.Alias"`) {
		t.Fatalf("expected unknown type id error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownJumpLabel(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []Instruction{{
			Op:      string(OpJump),
			Payload: json.RawMessage(`{"label":"missing"}`),
		}},
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown label") {
		t.Fatalf("expected unknown label error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownOpcode(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []Instruction{{
			Op: "invalid_opcode",
		}},
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown opcode") {
		t.Fatalf("expected unknown opcode error, got %v", err)
	}
}

func TestValidateInstructionRejectsDuplicateStructFields(t *testing.T) {
	payload, err := json.Marshal(MakeStructPayload{Type: testType("struct{Value:Int64}"), Fields: []string{"Value", "Value"}})
	if err != nil {
		t.Fatal(err)
	}
	err = validateInstruction("instruction", Instruction{Op: string(OpMakeStruct), Payload: payload})
	if err == nil || !strings.Contains(err.Error(), `duplicate struct field "Value"`) {
		t.Fatalf("expected duplicate struct field error, got %v", err)
	}
}

func TestValidateArtifactWithLimitsRejectsInstructionLimit(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{{ID: "c.zero", Type: testType("Int64"), Value: json.RawMessage(`0`)}}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Instructions: []Instruction{{
			Op:      string(OpConst),
			Payload: json.RawMessage(`{"constant":"c.zero"}`),
		}, {
			Op:      string(OpReturn),
			Payload: json.RawMessage(`{"result_count":1}`),
		}},
	}}
	limits := DefaultValidationLimits()
	limits.MaxInstructions = 1

	err := ValidateArtifactWithLimits(&artifact, limits)
	if err == nil || !strings.Contains(err.Error(), "instructions: limit exceeded") {
		t.Fatalf("expected instruction limit error, got %v", err)
	}
}
