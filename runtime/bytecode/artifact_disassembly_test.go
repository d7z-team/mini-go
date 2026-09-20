package bytecode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDisassembleProducesStableSnapshot(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{ID:Int64 `json:\"id\"`}"),
		Fields: []testTypeField{{
			Name: "ID",
			Type: testType("Int64"),
			Tag:  `json:"id"`,
		}},
	}})
	artifact.Requirements = []Requirement{{
		Kind:       "source",
		ModulePath: "test/dependency",
		Hash:       strings.Repeat("a", 64),
		Exports:    []string{"Add"},
	}}
	artifact.Constants = []Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Globals = []Global{{ID: "global.answer", Type: testType("Int64")}}
	artifact.Functions = []Function{{
		ID:           "fn.main",
		Signature:    testSignature("function() Int64"),
		Locals:       []Local{{ID: "local.tmp", Type: testType("Int64")}},
		ResultLocals: []string{"local.tmp"},
		Instructions: []Instruction{{
			Op:      string(OpConst),
			Payload: json.RawMessage(`{"constant":"c.answer"}`),
		}, {
			Op:      string(OpReturn),
			Payload: json.RawMessage(`{"result_count":1}`),
		}},
	}}
	artifact.Exports = []Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	attachTestTypeNodes(&artifact)
	got, err := Disassemble(&artifact)
	if err != nil {
		t.Fatalf("Disassemble failed: %v", err)
	}
	want := strings.Join([]string{
		"format mini-go-ir version 22 opcode_set minigo.ir.v10",
		"module example/module package main",
		"require source test/dependency hash " + strings.Repeat("a", 64) + " exports [Add]",
		"type type.User User = struct{ID:Int64 `json:\"id\"`}",
		`  field ID Int64 tag "json:\"id\""`,
		"const c.answer Int64 = 42",
		"global global.answer Int64",
		"export Main function fn.main",
		"func fn.main function() Int64",
		"  local local.tmp Int64",
		"  result_local local.tmp",
		`  0000 const {"constant":"c.answer"}`,
		`  0001 return {"result_count":1}`,
		"",
	}, "\n")
	if got != want {
		t.Fatalf("unexpected disassembly:\n%s", got)
	}
}

func TestDisassemblePreservesVariadicFunctionMetadata(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.Sum",
		Signature: testSignature("function(variadic Slice<Int64>) Int64"),
	}}
	attachTestTypeNodes(&artifact)

	disassembly, err := Disassemble(&artifact)
	if err != nil {
		t.Fatalf("Disassemble failed: %v", err)
	}
	if !strings.Contains(disassembly, "func fn.Sum function(variadic Slice<Int64>) Int64 [variadic]") {
		t.Fatalf("expected variadic function marker in disassembly, got:\n%s", disassembly)
	}
}

func TestDisassembleDerivesVariadicMetadataFromTypes(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "Holder",
		Type: testType("struct{Handler:function(Int64, variadic Slice<Int64>) Int64}"),
		Fields: []testTypeField{{
			Name: "Handler",
			Type: testType("function(Int64, variadic Slice<Int64>) Int64"),
		}},
	}})
	artifact.Globals = []Global{{ID: "global.handler", Type: testType("function(Int64, variadic Slice<Int64>) Int64")}}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Locals:    []Local{{ID: "local.handler", Type: testType("function(Int64, variadic Slice<Int64>) Int64")}},
		Upvalues:  []Upvalue{{ID: "up.handler", Type: testType("function(Int64, variadic Slice<Int64>) Int64")}},
	}}
	attachTestTypeNodes(&artifact)
	disassembly, err := Disassemble(&artifact)
	if err != nil {
		t.Fatalf("Disassemble failed: %v", err)
	}
	for _, want := range []string{
		"field Handler function(Int64, variadic Slice<Int64>) Int64 [variadic]",
		"global global.handler function(Int64, variadic Slice<Int64>) Int64",
		"local local.handler function(Int64, variadic Slice<Int64>) Int64",
		"upvalue up.handler function(Int64, variadic Slice<Int64>) Int64",
	} {
		if !strings.Contains(disassembly, want) {
			t.Fatalf("expected %q in disassembly, got:\n%s", want, disassembly)
		}
	}
}

func TestDisassembleShowsUntypedConstantExport(t *testing.T) {
	artifact := NewArtifact("example/module", "dep")
	artifact.Constants = []Constant{{ID: "const.Max", Type: testType("Int"), Value: json.RawMessage(`18446744073709551615`), Untyped: true}}
	artifact.Exports = []Export{{Name: "Max", Kind: "const", ID: "const.Max", Type: testType("Int"), Untyped: true}}
	got, err := Disassemble(&artifact)
	if err != nil {
		t.Fatalf("Disassemble failed: %v", err)
	}
	if !strings.Contains(got, "export Max const const.Max Int untyped") {
		t.Fatalf("expected untyped export marker, got:\n%s", got)
	}
	if !strings.Contains(got, "const const.Max Int = 18446744073709551615 [untyped]") {
		t.Fatalf("expected untyped constant marker, got:\n%s", got)
	}
}

func TestWriteDisassemblyMatchesDisassemble(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{ID: "fn.main", Signature: testSignature("function() Void")}}

	got, err := Disassemble(&artifact)
	if err != nil {
		t.Fatalf("Disassemble failed: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteDisassembly(&buf, &artifact); err != nil {
		t.Fatalf("WriteDisassembly failed: %v", err)
	}
	if buf.String() != got {
		t.Fatalf("expected WriteDisassembly to match Disassemble")
	}
}
