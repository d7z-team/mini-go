package parser_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
)

func TestPackageOwnsEmbedsAndTransfersFinalizedSyntaxOnce(t *testing.T) {
	files := []source.File{
		source.NewFile("a", "original.go", "package sample\r\nimport _ \"embed\"\r\n//go:embed input.txt\r\nvar 文本 string\r\n"),
		source.NewFile("b", "main.mgo", "package sample\nfunc Read() string { return 文本 }\n"),
	}
	builder := parser.NewPackageBuilder("sample", files, parser.Limits{})
	alias := builder
	embeds := builder.Embeds()
	if len(embeds) != 1 || !embeds[0].ImportsEmbed || !embeds[0].ValidVariable {
		t.Fatalf("embed metadata = %+v", embeds)
	}
	data := []byte("retained")
	if !builder.BindEmbed(embeds[0].File, embeds[0].Declaration, []ast.EmbedFile{{Path: "input.txt", Data: data}}) {
		t.Fatal("binding failed")
	}
	data[0] = 'x'
	embeds[0].Patterns[0] = "changed"
	files[0].LineStarts[0] = 99
	owned, imports, diagnostics := builder.Finalize()
	if len(diagnostics) != 0 || len(imports) != 0 {
		t.Fatalf("finalize: imports=%v diagnostics=%v", imports, diagnostics)
	}
	if alias.BindEmbed(0, 1, nil) {
		t.Fatal("consumed builder accepted another mutation")
	}
	copyOfOwned := owned
	program, documents, diagnostics := owned.Take(ast.Limits{})
	if len(diagnostics) != 0 || len(documents) != 2 {
		t.Fatalf("take: documents=%v diagnostics=%v", documents, diagnostics)
	}
	decl := program.Files[0].Decls[1].Var
	if got := string(decl.Values[0].EmbedFiles[0].Data); got != "retained" || decl.EmbedPatterns[0] != "input.txt" {
		t.Fatalf("caller mutation changed owned input: %+v", decl)
	}
	if decl.Values[0].NodeID == 0 || decl.NameIDs[0].Span.Start.Line != 4 || decl.NameIDs[0].Span.Start.File != "original.go" {
		t.Fatalf("embed identity or location = %+v", decl)
	}
	for _, document := range documents {
		validation, stats := ast.ValidateStructureWithStats(&document.Program, ast.Limits{})
		if len(validation) != 0 || document.NodeCount != stats.Nodes {
			t.Fatalf("document stats %d != %d, diagnostics=%v", document.NodeCount, stats.Nodes, validation)
		}
	}
	cloned := ast.CloneProgram(program)
	if diagnostics, _ := ast.FinalizeStructure(&cloned, ast.Limits{}); len(diagnostics) != 0 || !reflect.DeepEqual(program, cloned) {
		t.Fatalf("finalize identities are not stable: %v", diagnostics)
	}
	_, _, diagnostics = copyOfOwned.Take(ast.Limits{})
	if len(diagnostics) != 1 || diagnostics[0].Code != "ast.program.missing" {
		t.Fatalf("second transfer = %v", diagnostics)
	}
}

func TestPackageRequiresFinalizationBeforeSemanticTransfer(t *testing.T) {
	builder := parser.NewPackageBuilder("sample", []source.File{source.NewFile("main", "main.mgo", "package sample\nvar Answer = 42\n")}, parser.Limits{})
	if reflect.TypeOf(builder).ConvertibleTo(reflect.TypeFor[parser.Package]()) {
		t.Fatal("Go type conversion can bypass structural validation and identity assignment")
	}
}

func TestPackageFinalizationIncludesAllFilesAndConsumerLimits(t *testing.T) {
	files := []source.File{source.NewFile("a", "a.mgo", "package p\nvar A = 1\n"), source.NewFile("b", "b.mgo", "package p\nvar B = 2\n")}
	builder := parser.NewPackageBuilder("p", files, parser.Limits{})
	owned, _, diagnostics := builder.Finalize()
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	program, _, _ := owned.Take(ast.Limits{})
	_, stats := ast.ValidateStructureWithStats(&program, ast.Limits{})
	for _, consumerLimit := range []bool{false, true} {
		limits := parser.Limits{}
		if !consumerLimit {
			limits.MaxASTNodes = stats.Nodes - 1
		}
		owned, _, diagnostics = parser.NewPackageBuilder("p", files, limits).Finalize()
		if consumerLimit {
			if len(diagnostics) != 0 {
				t.Fatal(diagnostics)
			}
			_, _, diagnostics = owned.Take(ast.Limits{MaxNodes: stats.Nodes - 1})
		}
		if len(diagnostics) != 1 || diagnostics[0].Code != "ast.limit.nodes" {
			t.Fatalf("consumer=%v diagnostics=%v", consumerLimit, diagnostics)
		}
	}
}

func TestPackageStructuralDiagnosticsRetainSourceAndConsumerBudget(t *testing.T) {
	file := source.NewFile("a", "a.mgo", "package sample\nvar A = 1 + 2 + 3 + 4 + 5\n")
	owned, _, diagnostics := parser.NewPackageBuilder("sample", []source.File{file}, parser.Limits{MaxASTNodes: 3}).Finalize()
	if len(diagnostics) == 0 || diagnostics[0].Code != "ast.limit.nodes" || diagnostics[0].ModulePath != "sample" || diagnostics[0].Primary.Start.File != "a.mgo" {
		t.Fatalf("structural source = %v", diagnostics)
	}
	_, documents, _ := owned.Take(ast.Limits{})
	if len(documents[0].Diagnostics) == 0 || documents[0].Diagnostics[0].ModulePath != "sample" {
		t.Fatalf("document source = %v", documents[0].Diagnostics)
	}
	files := []source.File{source.NewFile("a", "a.mgo", "package sample\nvar =\n"), source.NewFile("b", "b.mgo", "package sample\nvar =\n")}
	owned, _, _ = parser.NewPackageBuilder("sample", files, parser.Limits{}).Finalize()
	_, _, diagnostics = owned.Take(ast.Limits{MaxDiagnostics: 1})
	if len(diagnostics) != 2 || diagnostics[1].Code != source.DiagnosticTruncated {
		t.Fatalf("consumer diagnostic limit = %v", diagnostics)
	}
}

func BenchmarkPackageFinalization(b *testing.B) {
	text := "package sample\nfunc Run(x int) int {\n" + strings.Repeat("x = x * 2 + 1\n", 100) + "return x\n}\n"
	file := source.NewFile("main", "main.mgo", text)
	for _, owned := range []bool{false, true} {
		name := "external_ast"
		if owned {
			name = "owned_package"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if owned {
					pkg, _, diagnostics := parser.NewPackageBuilder("sample", []source.File{file}, parser.Limits{}).Finalize()
					if len(diagnostics) != 0 {
						b.Fatal(diagnostics)
					}
					_, _, _ = pkg.Take(ast.Limits{})
				} else {
					parsed := parser.ParseDocumentFile("sample", file)
					if len(parsed.Diagnostics) != 0 {
						b.Fatal(parsed.Diagnostics)
					}
					if diagnostics, _ := ast.FinalizeStructure(&parsed.Program, ast.Limits{}); len(diagnostics) != 0 {
						b.Fatal(diagnostics)
					}
				}
			}
		})
	}
}
