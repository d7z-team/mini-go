package compiler

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestCompilerSourceCapacityCanBeRaised(t *testing.T) {
	text := "package main\n/*" + strings.Repeat("x", scanner.DefaultMaxSourceBytes) + "*/\nfunc Main() int { return 42 }\n"
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: text}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	storage := cache.New(cache.NewMemoryBackend())
	for _, limit := range []int{0, len(text), 0} {
		result, err := Check(Request{Root: "example/main", Sources: sources, Cache: storage, Limits: Limits{MaxSourceBytes: limit}})
		if err != nil {
			t.Fatal(err)
		}
		if limit == 0 {
			requireCompileDiagnostic(t, result.Diagnostics, "scanner.source.limit")
		} else if len(result.Diagnostics) != 0 {
			t.Fatalf("raised source capacity: %v", result.Diagnostics)
		}
	}
}

func TestCompilerAppliesWorkspaceLimits(t *testing.T) {
	packages := []workspace.SourcePackage{
		{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/lib\"\nfunc main() {}\n"}, {Path: "extra.mgo", Text: "package main\n"}}},
		{ModulePath: "example/lib", Files: []source.File{{Path: "lib.mgo", Text: "package lib\n"}}},
	}
	sources, err := workspace.NewMemorySourceSet(packages)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		limits Limits
		code   source.DiagnosticCode
	}{
		{name: "packages", limits: Limits{MaxPackages: 1}, code: "compiler.limit.packages"},
		{name: "files", limits: Limits{MaxFiles: 1}, code: "compiler.limit.files"},
		{name: "total bytes", limits: Limits{MaxTotalSourceBytes: 16}, code: "compiler.limit.source_bytes"},
		{name: "source bytes", limits: Limits{MaxSourceBytes: 16}, code: "scanner.source.limit"},
		{name: "tokens", limits: Limits{MaxTokens: 4}, code: "scanner.token.limit"},
		{name: "AST nodes", limits: Limits{MaxASTNodes: 4}, code: "ast.limit.nodes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Check(Request{Root: "example/main", Sources: sources, Limits: test.limits})
			if err != nil {
				t.Fatal(err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, string(test.code))
		})
	}
}

func TestCompilerBoundsDiagnostics(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc broken() { @ @ @ @ @ @ @ @ }\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Check(Request{Root: "example/main", Sources: sources, Limits: Limits{MaxDiagnostics: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) > 3 {
		t.Fatalf("diagnostic budget exceeded: %#v", result.Diagnostics)
	}
	requireCompileDiagnostic(t, result.Diagnostics, string(source.DiagnosticTruncated))
}

func TestCompilerAppliesGenericSpecializationLimit(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files: []source.File{{Path: "main.mgo", Text: `package main
func Identity[T any](value T) T { return value }
func main() {
	_ = Identity[int](1)
	_ = Identity[string]("")
}
`}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Compile(Request{Root: "example/main", Sources: sources, Limits: Limits{MaxSpecializations: 1}})
	if err != nil {
		t.Fatal(err)
	}
	requireCompileDiagnostic(t, result.Diagnostics, "compiler.generic.limit")
}
