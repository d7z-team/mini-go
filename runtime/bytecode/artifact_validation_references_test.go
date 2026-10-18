package bytecode

import (
	"strings"
	"testing"
)

func TestComparisonBranchTargets(t *testing.T) {
	for _, op := range []Opcode{OpCompareBranch, OpLenCompareBranch} {
		for _, target := range []string{"done", "missing"} {
			t.Run(op.String()+"/"+target, func(t *testing.T) {
				source := "Int"
				if op == OpLenCompareBranch {
					source = "String"
				}
				artifact := NewArtifact("example/module", "main")
				artifact.Functions = []Function{{ID: "main", Code: testMicroCode([]string{source, "Int"}, []Instruction{
					{Op: OpZero, Payload: TypePayload{Type: testType(source)}},
					{Op: OpZero, Payload: TypePayload{Type: testType("Int")}},
					{Op: op, Payload: CompareBranchPayload{Type: testType("Int"), Operator: "==", Label: target}},
					{Op: OpLabel, Payload: LabelPayload{Label: "done"}},
					{Op: OpReturn, Payload: ReturnPayload{}},
				}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, nil}, {nil, nil}, {nil, nil}})}}
				err := testValidateArtifact(&artifact)
				if target == "done" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				issue, ok := ValidationIssueFromError(err)
				if !ok || issue.Code != ValidationLabelUnknown || issue.Path != "functions[0].instructions[2]" {
					t.Fatalf("unknown target diagnostic = %v", err)
				}
			})
		}
	}
}

func TestValidateArtifactRejectsUnknownAddressLocal(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Ptr<Int64>"),
		Code: testMicroCode([]string{"Ptr<Int64>"}, []Instruction{{
			Op:      OpAddressOf,
			Payload: AddressPayload{Kind: "local", Local: "local.missing"},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown local") {
		t.Fatalf("expected unknown local error, got %v", err)
	}
}

func TestValidateArtifactRejectsRebindOnLoadLocal(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Locals:    []Local{{ID: "local.value", Type: testType("Int64")}},
		Code: testMicroCode([]string{"Int64"}, []Instruction{{
			Op:      OpLoadLocal,
			Payload: LocalPayload{Local: "local.value", Rebind: true},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "rebind is only valid for store_local") {
		t.Fatalf("expected invalid rebind payload error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownDirectCall(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{}, []Instruction{{
			Op:      OpCallDirect,
			Payload: CallPayload{Function: "fn.missing", ArgCount: 0},
		}}, [][2][]uint32{{nil, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown function id") {
		t.Fatalf("expected unknown function id error, got %v", err)
	}
}

func TestValidateArtifactRejectsDirectCallShapeMismatch(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID: "fn.main", Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"Int"}, []Instruction{{
			Op: OpCallDirect, Payload: CallPayload{Function: "fn.target", ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}}),
	}, {
		Code: &MicroCode{},
		ID:   "fn.target", Signature: testSignature("function(Int) Void"),
	}}
	if err := testValidateArtifact(&artifact); err == nil || !strings.Contains(err.Error(), "call argument count mismatch") {
		t.Fatalf("ValidateArtifact() = %v, want call argument count mismatch", err)
	}
}

func TestValidateArtifactRejectsReturnShapeMismatch(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID: "fn.main", Signature: testSignature("function() Int"),
		Code: testMicroCode([]string{}, []Instruction{{
			Op: OpReturn, Payload: ReturnPayload{},
		}}, [][2][]uint32{{nil, nil}}),
	}}
	if err := testValidateArtifact(&artifact); err == nil || !strings.Contains(err.Error(), "return result count mismatch") {
		t.Fatalf("ValidateArtifact() = %v, want return result count mismatch", err)
	}
}

func TestValidateArtifactAcceptsCrossModuleDirectCallRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Requirements = []Requirement{{Kind: "source", ModulePath: "example/lib"}}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{}, []Instruction{{
			Op:      OpCallDirect,
			Payload: CallPayload{ModulePath: "example/lib", Function: "method.Counter.Add", ArgCount: 0},
		}}, [][2][]uint32{{nil, nil}}),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactAcceptsLocalModuleDirectCallPayload(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{}, []Instruction{{
			Op:      OpCallDirect,
			Payload: CallPayload{ModulePath: "example/module", Function: "fn.helper", ArgCount: 0},
		}}, [][2][]uint32{{nil, nil}}),
	}, {
		Code:      &MicroCode{},
		ID:        "fn.helper",
		Signature: testSignature("function() Void"),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactRejectsCrossModuleDirectCallWithoutRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{}, []Instruction{{
			Op:      OpCallDirect,
			Payload: CallPayload{ModulePath: "example/lib", Function: "method.Counter.Add", ArgCount: 0},
		}}, [][2][]uint32{{nil, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown module requirement") {
		t.Fatalf("expected unknown module requirement error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownClosureFunction(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"function() Void"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{Function: "fn.missing"},
		}}, [][2][]uint32{{nil, {0}}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown function id") {
		t.Fatalf("expected unknown function id error, got %v", err)
	}
}

func TestValidateArtifactAcceptsCrossModuleClosureRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Requirements = []Requirement{{Kind: "source", ModulePath: "example/lib"}}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"function() Void"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{ModulePath: "example/lib", Function: "method.Counter.Add"},
		}, {
			Op: OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactRejectsCrossModuleClosureWithoutRequirement(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"function() Void"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{ModulePath: "example/lib", Function: "method.Counter.Add"},
		}}, [][2][]uint32{{nil, {0}}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown module requirement") {
		t.Fatalf("expected unknown module requirement error, got %v", err)
	}
}

func TestValidateArtifactRejectsUnknownUpvalueSlot(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Code: testMicroCode([]string{"Int64"}, []Instruction{{
			Op:      OpLoadUpvalue,
			Payload: UpvaluePayload{Upvalue: "up.missing"},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown upvalue") {
		t.Fatalf("expected unknown upvalue error, got %v", err)
	}
}

func TestValidateArtifactRejectsClosureCaptureCountMismatch(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.child",
		Signature: testSignature("function() Int64"),
		Upvalues:  []Upvalue{{ID: "up.x", Type: testType("Int64")}},
		Code: testMicroCode([]string{"Int64"}, []Instruction{{
			Op:      OpLoadUpvalue,
			Payload: UpvaluePayload{Upvalue: "up.x"},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"function() Int64"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{Function: "fn.child"},
		}, {
			Op: OpPop,
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "capture count mismatch") {
		t.Fatalf("expected capture count mismatch error, got %v", err)
	}
}

func TestValidateArtifactRejectsSpawnResultCount(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		Code:      &MicroCode{},
		ID:        "fn.child",
		Signature: testSignature("function() Void"),
	}, {
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testMicroCode([]string{"function() Void"}, []Instruction{{
			Op:      OpMakeClosure,
			Payload: ClosurePayload{Function: "fn.child"},
		}, {
			Op:      OpSpawn,
			Payload: CallPayload{ArgCount: 0, ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "spawn must not produce results") {
		t.Fatalf("expected spawn result count error, got %v", err)
	}
}
