package semantic

import (
	"encoding/json"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestArrayLengthsUseExactConstantFacts(t *testing.T) {
	parsed := parser.ParseSource("example/arrays", "arrays.mgo", `package arrays

const Later = Base + 1
const Base = 31

type Digest [Later]byte
type Shifted [1 << 5]byte
type Empty [0]byte

func Local() {
	const Size = 4
	var values [Size]int
	_ = values
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatalf("check source: %#v", checked.Info.Diagnostics)
	}

	want := map[string]int64{"Digest": 32, "Shifted": 32, "Empty": 0}
	for _, decl := range checked.Program.Files[0].Decls {
		if decl.Kind != ast.DeclType {
			continue
		}
		ref := checked.Info.Types[decl.Type.Type.NodeID].Type
		length, _, ok := types.View(checked.Info.TypeTable, ref).Array()
		if !ok || length != want[decl.Type.Name] {
			t.Fatalf("%s length = %d, %v; want %d", decl.Type.Name, length, ok, want[decl.Type.Name])
		}
	}

	local := &checked.Program.Files[0].Decls[5].Func.Body.Stmts[1].Decls[0]
	ref := checked.Info.Types[local.Var.Type.NodeID].Type
	if length, _, ok := types.View(checked.Info.TypeTable, ref).Array(); !ok || length != 4 {
		t.Fatalf("local array length = %d, %v", length, ok)
	}
}

func TestArrayLengthsUseLenAndCapConstantFacts(t *testing.T) {
	parsed := parser.ParseSource("example/arrays", "builtin_lengths.mgo", `package arrays

const FixedLen = len([3]int{})
const FixedCap = cap([4]int{})
const InferredLen = len([... ]int{1, 4: 2})
const PointerCap = cap(&[6]int{})
type Result [FixedLen + FixedCap + InferredLen + PointerCap]int
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatalf("check source: %#v", checked.Info.Diagnostics)
	}
	decl := checked.Program.Files[0].Decls[4]
	ref := checked.Info.Types[decl.Type.Type.NodeID].Type
	if length, _, ok := types.View(checked.Info.TypeTable, ref).Array(); !ok || length != 18 {
		t.Fatalf("Result length = %d, %v; want 18", length, ok)
	}
}

func TestArrayLengthUsesLenOfInferredArrayVariable(t *testing.T) {
	parsed := parser.ParseSource("example/arrays", "variable_length.mgo", `package arrays

var sizes = [...]int{128, 512, 2048, 16384, 0}
var pools [len(sizes)]int
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatalf("check source: %#v", checked.Info.Diagnostics)
	}
	decl := checked.Program.Files[0].Decls[1]
	ref := checked.Info.Types[decl.Var.Type.NodeID].Type
	if length, _, ok := types.View(checked.Info.TypeTable, ref).Array(); !ok || length != 5 {
		t.Fatalf("pools length = %d, %v; want 5", length, ok)
	}
}

func TestArrayLengthDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		code   string
	}{
		{"non constant", "package p\nvar N = 2\ntype A [N]int", "semantic.array.length.constant"},
		{"non integer", "package p\nconst N = 1.5\ntype A [N]int", "semantic.array.length.integer"},
		{"negative", "package p\nconst N = -1\ntype A [N]int", "semantic.array.length.negative"},
		{"overflow", "package p\nconst N = 1 << 63\ntype A [N]int", "semantic.array.length.overflow"},
		{"cycle", "package p\nconst A = B\nconst B = A\ntype C [A]int", "semantic.const.cycle"},
		{"len operand call", "package p\nfunc Value() int { return 1 }\nconst N = len([1]int{Value()})\ntype A [N]int", "semantic.array.length.constant"},
		{"cap operand receive", "package p\nvar Values chan int\nconst N = cap(&[1]int{<-Values})\ntype A [N]int", "semantic.array.length.constant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed := parser.ParseSource("example/p", "p.mgo", test.source)
			if len(parsed.Diagnostics) != 0 {
				t.Fatalf("parse source: %#v", parsed.Diagnostics)
			}
			info := Check(parsed.Program).Info
			for _, diagnostic := range info.Diagnostics {
				if string(diagnostic.Code) == test.code {
					return
				}
			}
			t.Fatalf("missing diagnostic %s: %#v", test.code, info.Diagnostics)
		})
	}
}

func TestArrayLengthsUseImportedConstantFacts(t *testing.T) {
	for _, source := range []string{
		"package arrays\nimport \"example/sizes\"\ntype Digest [sizes.Size]byte",
		"package arrays\nimport . \"example/sizes\"\ntype Digest [Size]byte",
	} {
		parsed := parser.ParseSource("example/arrays", "arrays.mgo", source)
		if len(parsed.Diagnostics) != 0 {
			t.Fatalf("parse source: %#v", parsed.Diagnostics)
		}
		checked := WithOptions(parsed.Program, AnalyzeOptions{Dependencies: testDependencies([]DependencyExport{{
			ModulePath: "example/sizes", Name: "Size", Kind: ObjectConst, Type: "Int",
			Value: json.RawMessage("32"), Untyped: true,
		}})})
		if len(checked.Info.Diagnostics) != 0 {
			t.Fatalf("check source: %#v", checked.Info.Diagnostics)
		}
		decl := checked.Program.Files[0].Decls[1]
		ref := checked.Info.Types[decl.Type.Type.NodeID].Type
		if length, _, ok := types.View(checked.Info.TypeTable, ref).Array(); !ok || length != 32 {
			t.Fatalf("imported array length = %d, %v", length, ok)
		}
	}
}

func TestImportedConstantSnapshotsFollowDependencyRevision(t *testing.T) {
	const input = "package arrays\nimport \"example/sizes\"\nconst Doubled = sizes.Size + sizes.Size\ntype Digest [Doubled]byte"
	options := AnalyzeOptions{Dependencies: testDependencies([]DependencyExport{{
		ModulePath: "example/sizes", Name: "Size", Kind: ObjectConst, Type: "Int",
		Value: json.RawMessage("16"), Untyped: true,
	}})}
	var snapshots []CheckedProgram
	for _, value := range []string{"16", "32"} {
		options.Dependencies[0].Members[0].Value = json.RawMessage(value)
		parsed := parser.ParseSource("example/arrays", "arrays.mgo", input)
		checked := WithOptions(parsed.Program, options)
		if len(checked.Info.Diagnostics) != 0 {
			t.Fatal(checked.Info.Diagnostics)
		}
		snapshots = append(snapshots, checked)
	}
	for i, checked := range snapshots {
		decl := checked.Program.Files[0].Decls[2]
		ref := checked.Info.Types[decl.Type.Type.NodeID].Type
		if length, _, ok := types.View(checked.Info.TypeTable, ref).Array(); !ok || length != int64(32*(i+1)) {
			t.Fatalf("dependency revision %d: array length %d, %v", i, length, ok)
		}
	}
}
