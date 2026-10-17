package bytecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateMinMaxOperandContract(t *testing.T) {
	for _, test := range []struct {
		name, left, right, output, operator string
		count                               int
		valid                               bool
	}{
		{"integer", "Int", "Int", "Int", "min", 2, true},
		{"string", "String", "String", "String", "max", 2, true},
		{"operator", "Int", "Int", "Int", "sum", 2, false},
		{"empty", "Int", "Int", "Int", "min", 0, false},
		{"arity", "Int", "Int", "Int", "min", 3, false},
		{"boolean", "Bool", "Bool", "Bool", "min", 2, false},
		{"complex", "Complex64", "Complex64", "Complex64", "min", 2, false},
		{"input type", "Int", "Float64", "Int", "min", 2, false},
		{"output type", "Int", "Int", "Float64", "min", 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := NewArtifact("minmax/contract", "main")
			artifact.Functions = []Function{{
				ID: "fn.main", Signature: testSignature("function() " + test.output),
				Code: testSlotCode([]string{test.left, test.right, test.output}, []Instruction{
					{Op: OpZero, Payload: TypePayload{Type: testType(test.left)}},
					{Op: OpZero, Payload: TypePayload{Type: testType(test.right)}},
					{Op: OpMinMax, Payload: MinMaxPayload{Type: testType(test.left), Operator: test.operator, ArgCount: test.count}},
					{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
				}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
			}}
			err := testValidateArtifact(&artifact)
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

func TestValidateArtifactAcceptsTypedInterfaceCallOperands(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{{ID: "c.delta", Type: testType("Int64"), Value: json.RawMessage(`2`)}}
	setTestNamedTypes(&artifact, []testNamedType{{Name: "Adder", Type: testType("interface{Add:function(Int64) Int64}")}})
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Locals:    []Local{{ID: "local.a", Type: testType("Adder")}},
		Code: testSlotCode([]string{"Adder", "Int64", "Int64"}, []Instruction{{
			Op:      OpLoadLocal,
			Payload: LocalPayload{Local: "local.a"},
		}, {
			Op:      OpConst,
			Payload: ConstPayload{Constant: "c.delta"},
		}, {
			Op:      OpCallInterface,
			Payload: CallInterfacePayload{InterfaceType: testType("Adder"), Method: "Add", ArgCount: 1, ResultCount: 1},
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 1},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}}),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
	disassembly, err := Disassemble(&artifact)
	if err != nil {
		t.Fatalf("Disassemble failed: %v", err)
	}
	if !strings.Contains(disassembly, "call_interface") || !strings.Contains(disassembly, `"interface_type":{"kind":4`) {
		t.Fatalf("expected call_interface in disassembly, got:\n%s", disassembly)
	}
}

func TestValidateArtifactRejectsInvalidCallInterfacePayload(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Any"}, []Instruction{{
			Op:      OpCallInterface,
			Payload: CallInterfacePayload{InterfaceType: testType("Adder"), ArgCount: 0},
		}}, [][2][]uint32{{{0}, nil}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "missing method") {
		t.Fatalf("expected missing method error, got %v", err)
	}
}

func TestValidateArtifactAcceptsValueAndPresenceDestinations(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{
		{ID: "c.key", Type: testType("String"), Value: json.RawMessage(`"k"`)},
		{ID: "c.value", Type: testType("Int64"), Value: json.RawMessage(`42`)},
	}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"String", "Int64", "Map<String, Int64>", "Bool"}, []Instruction{{
			Op:      OpConst,
			Payload: ConstPayload{Constant: "c.key"},
		}, {
			Op:      OpConst,
			Payload: ConstPayload{Constant: "c.value"},
		}, {
			Op:      OpMakeMap,
			Payload: MakeMapPayload{Type: testType("Map<String, Int64>"), EntryCount: 1},
		}, {
			Op:      OpConst,
			Payload: ConstPayload{Constant: "c.key"},
		}, {
			Op: OpLoadIndexOK,
		}, {
			Op: OpPop,
		}, {
			Op: OpPop,
		}, {
			Op:      OpReturn,
			Payload: ReturnPayload{ResultCount: 0},
		}}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {nil, {0}}, {{2, 0}, {1, 3}}, {{3}, nil}, {{1}, nil}, {nil, nil}}),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}
