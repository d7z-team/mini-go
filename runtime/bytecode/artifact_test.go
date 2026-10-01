package bytecode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestValidateArtifactAcceptsMinimalArtifact(t *testing.T) {
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

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactAppliesTypeLimitBeforeGraphValidation(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.TypeTable.Nodes = []types.TypeNode{
		{ID: "alias.a", Kind: types.Named, Identity: types.TypeKey{ModulePath: "example/module", DeclID: "A"}, Alias: true, AliasTarget: types.TypeRef{Kind: types.Named, Node: "alias.b"}},
		{ID: "alias.b", Kind: types.Named, Identity: types.TypeKey{ModulePath: "example/module", DeclID: "B"}, Alias: true, AliasTarget: types.TypeRef{Kind: types.Named, Node: "alias.a"}},
	}
	if err := artifact.TypeTable.Reindex(); err != nil {
		t.Fatal(err)
	}
	err := ValidateArtifactWithLimits(&artifact, ValidationLimits{MaxTypes: 1})
	if err == nil || !strings.Contains(err.Error(), "types: limit exceeded") {
		t.Fatalf("ValidateArtifactWithLimits() = %v, want type limit", err)
	}
}

func TestValidateArtifactRejectsDuplicateFunctionID(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{
		{Code: &SlotCode{}, ID: "fn.main", Signature: testSignature("function() Void")},
		{Code: &SlotCode{}, ID: "fn.main", Signature: testSignature("function() Void")},
	}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
}

func TestValidateArtifactChecksFunctionResultLocals(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:           "fn.main",
		Signature:    testSignature("function() Int64"),
		Locals:       []Local{{ID: "local.out", Type: testType("Int64")}},
		ResultLocals: []string{"local.out"},
		Code: testSlotCode([]string{"Int64"}, []Instruction{{
			Op:      OpLoadLocal,
			Payload: LocalPayload{Local: "local.out"},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}

	artifact.Functions[0].ResultLocals = []string{"local.missing"}
	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown local slot") {
		t.Fatalf("expected unknown result local error, got %v", err)
	}

	artifact.Functions[0].ResultLocals = []string{"local.out", "local.out"}
	err = testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "result local count mismatch") {
		t.Fatalf("expected result local count mismatch, got %v", err)
	}
}

func TestValidateArtifactRejectsMissingPayloadField(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int"}, []Instruction{{
			Op:      OpConst,
			Payload: ConstPayload{},
		}}, [][2][]uint32{{nil, {0}}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "missing constant id") {
		t.Fatalf("expected missing constant id error, got %v", err)
	}
	if code := ValidationCode(err); code != "ir.field.missing" {
		t.Fatalf("expected missing field code, got %q", code)
	}
}

func TestValidateArtifactRejectsMissingTypePayload(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int", "Int"}, []Instruction{{
			Op:      OpConvert,
			Payload: TypePayload{},
		}}, [][2][]uint32{{{0}, {1}}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "missing type") {
		t.Fatalf("expected missing type error, got %v", err)
	}
}

func TestValidateArtifactRejectsPayloadForNoPayloadOpcode(t *testing.T) {
	err := validateInstruction("instruction", &Instruction{Op: OpPop, Payload: ReturnPayload{}})
	if err == nil || !strings.Contains(err.Error(), "must not have payload") {
		t.Fatalf("expected no-payload opcode error, got %v", err)
	}
	if code := ValidationCode(err); code != "ir.payload.unexpected" {
		t.Fatalf("expected unexpected payload code, got %q", code)
	}
}

func TestValidateArtifactAcceptsDeferOwnerDepthPayload(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		Code:      &SlotCode{},
		ID:        "fn.cleanup",
		Signature: testSignature("function() Void"),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{Function: "fn.cleanup"},
		}, {
			Op:      OpDeferPush,
			Payload: DeferPayload{OwnerDepth: 2},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 0},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactRejectsNegativeDeferOwnerDepth(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		Code:      &SlotCode{},
		ID:        "fn.cleanup",
		Signature: testSignature("function() Void"),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{Function: "fn.cleanup"},
		}, {
			Op:      OpDeferPush,
			Payload: DeferPayload{OwnerDepth: -1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "owner depth must be non-negative") {
		t.Fatalf("expected owner depth error, got %v", err)
	}
}

func TestValidateArtifactRejectsMissingWaitableTypePayload(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int", "Any"}, []Instruction{{
			Op:      OpMakeWaitable,
			Payload: MakeWaitablePayload{},
		}}, [][2][]uint32{{{0}, {1}}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "missing type") {
		t.Fatalf("expected missing make_waitable type error, got %v", err)
	}
	if code := ValidationCode(err); code != "ir.field.missing" {
		t.Fatalf("expected missing field code, got %q", code)
	}
}

func TestValidateArtifactPreservesNestedVariadicFunctionTypes(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "Holders",
		Type: testType("struct{Handlers:Slice<function(variadic Slice<Int64>) Int64>}"),
		Fields: []testTypeField{{
			Name: "Handlers",
			Type: testType("Slice<function(variadic Slice<Int64>) Int64>"),
		}},
	}})
	artifact.Globals = []Global{{
		ID:   "global.handlers",
		Type: testType("Map<String, function(variadic Slice<Int64>) Int64>"),
	}}
	artifact.Functions = []Function{{Code: &SlotCode{}, ID: "fn.main", Signature: testSignature("function() Void")}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact rejected nested variadic function type: %v", err)
	}
}

func TestValidateArtifactRejectsNonFinalNestedVariadicFunctionType(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "Bad",
		Type: testInvalidNestedVariadicType(),
	}})
	artifact.Functions = []Function{{Code: &SlotCode{}, ID: "fn.main", Signature: testSignature("function() Void")}}

	if err := testValidateArtifact(&artifact); err == nil || !strings.Contains(err.Error(), "invalid function type") {
		t.Fatalf("expected invalid nested variadic function type error, got %v", err)
	}
}

func TestValidateArtifactAcceptsLocalModuleMethodMetadata(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "Token",
		Type: testType("struct{}"),
		Methods: []testTypeMethod{{
			Name:       "hidden",
			Receiver:   testType("Token"),
			Signature:  testSignature("function() Int"),
			FunctionID: "method.Token.hidden",
			ModulePath: "example/module",
		}},
	}})
	artifact.Functions = []Function{{
		Code:      &SlotCode{},
		ID:        "method.Token.hidden",
		Signature: testSignature("function() Int"),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactRejectsDuplicateMethodMetadata(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "Reader",
		Type: testType("interface{Read:function() Int}"),
		Methods: []testTypeMethod{
			{Name: "Read", Receiver: testType("Reader"), Signature: testSignature("function() Int")},
			{Name: "Read", Receiver: testType("Reader"), Signature: testSignature("function() Int")},
		},
	}})

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "duplicate method identity") {
		t.Fatalf("expected duplicate method metadata error, got %v", err)
	}
	if code := ValidationCode(err); code != "ir.type.method.duplicate" {
		t.Fatalf("expected duplicate method validation code, got %s", code)
	}
}

func TestValidateArtifactRejectsUnknownMethodModuleRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "Token",
		Type: testType("struct{}"),
		Methods: []testTypeMethod{{
			Name:       "hidden",
			Receiver:   testType("Token"),
			Signature:  testSignature("function() Int"),
			FunctionID: "method.Token.hidden",
			ModulePath: "example/lib",
		}},
	}})

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown module requirement") {
		t.Fatalf("expected unknown module requirement error, got %v", err)
	}
}
