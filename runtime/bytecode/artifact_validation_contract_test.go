package bytecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidationReadsPayloadEditsBetweenAnalyses(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code:      testSlotCode([]string{}, []Instruction{{Op: OpReturn, Payload: ReturnPayload{ResultCount: 0}}}, [][2][]uint32{{nil, nil}}),
	}}
	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatal(err)
	}
	code := artifact.Functions[0].Code
	for _, count := range []int{1, -1} {
		code.Descriptors.Return[0].ResultCount = count
		if err := testValidateArtifact(&artifact); err == nil {
			t.Fatalf("accepted modified result count %d", count)
		}
	}
	code.Descriptors.Return[0].ResultCount = 0
	code.Instructions[0].Descriptor = 1
	if err := testValidateArtifact(&artifact); err == nil {
		t.Fatal("accepted out-of-range descriptor after a prior successful validation")
	}
	code.Instructions[0].Descriptor = 0
	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatal(err)
	}
}

func TestValidateArtifactRejectsUnknownExportRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Any"}, []Instruction{{
			Op:      OpLoadExport,
			Payload: ExportPayload{ModulePath: "example/lib", Export: "Missing"},
		}, {
			Op: OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
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
		Code: testSlotCode([]string{}, []Instruction{{
			Op:      OpInitModule,
			Payload: InitModulePayload{ModulePath: "example/lib"},
		}}, [][2][]uint32{{nil, nil}}),
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
		Code: testSlotCode([]string{}, []Instruction{{
			Op:      OpJump,
			Payload: JumpPayload{Label: "missing"},
		}}, [][2][]uint32{{nil, nil}}),
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
		Code: testSlotCode([]string{}, []Instruction{{
			Op: Opcode(65535),
		}}, [][2][]uint32{{nil, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown opcode") {
		t.Fatalf("expected unknown opcode error, got %v", err)
	}
}

func TestValidateInstructionRejectsDuplicateStructFields(t *testing.T) {
	payload := MakeStructPayload{Type: testType("struct{Value:Int64}"), Fields: []string{"Value", "Value"}}
	err := validateInstruction("instruction", &Instruction{Op: OpMakeStruct, Payload: payload})
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
		Code: testSlotCode([]string{"Int64"}, []Instruction{{
			Op:      OpConst,
			Payload: ConstPayload{Constant: "c.zero"},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	limits := DefaultValidationLimits()
	limits.MaxInstructions = 1

	err := ValidateArtifactWithLimits(&artifact, limits)
	if err == nil || !strings.Contains(err.Error(), "instructions: limit exceeded") {
		t.Fatalf("expected instruction limit error, got %v", err)
	}
}
