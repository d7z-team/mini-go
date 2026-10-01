package compiler_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestPrepareReusesAnalysisWithoutChangingPublishedSyntax(t *testing.T) {
	for name, body := range map[string]string{
		"method":      `func Main() int { return dep.Value{N:42}.Read() }`,
		"local types": `func Main() int { type Number int; type Alias = Number; n := Alias(42); return func() int { return int(n) }() }`,
		"generic":     `func Main() int { return dep.Keep[int](42) }`,
		"interface":   `func Main() int { var reader interface { Read() int } = dep.Value{N:42}; return reader.Read() }`,
	} {
		t.Run(name, func(t *testing.T) {
			set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
				{ModulePath: "dep", Files: []source.File{{Path: "dep.mgo", Text: "package dep\ntype Value struct { N int }\nfunc (v Value) Read() int { return v.N }\nfunc Keep[T any](v T) T { return v }"}}},
				{ModulePath: "app", Files: []source.File{{Path: "app.mgo", Text: "package app\nimport \"dep\"\nvar _ = dep.Value{}\n" + body}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := compiler.Request{Root: "app", Sources: set, Symbols: true, EntryPoints: []compiler.EntryPoint{{Name: "default", ModulePath: "app", Function: "Main"}}}
			analyzed, err := compiler.Analyze(compiler.AnalysisRequest{Request: request})
			if err != nil || source.HasErrors(analyzed.Diagnostics) {
				t.Fatalf("analyze: %v %v", err, analyzed.Diagnostics)
			}
			before, err := json.Marshal(analyzed.Packages["app"].Checked.Program)
			if err != nil {
				t.Fatal(err)
			}
			cold, err := compiler.Prepare(request)
			if err != nil || cold.Image == nil {
				t.Fatalf("cold prepare: %v %v", err, cold.Checked.Diagnostics)
			}
			request.PreviousAnalysis = &analyzed
			for range 2 {
				warm, err := compiler.Prepare(request)
				if err != nil || warm.Image == nil {
					t.Fatalf("reused prepare: %v %v", err, warm.Checked.Diagnostics)
				}
				if warm.Image.Hash != cold.Image.Hash || warm.Symbols.Hash != cold.Symbols.Hash {
					t.Fatal("analysis reuse changed executable or debug symbols")
				}
				if warm.Checked.Stats.PackagesParsed != 0 || warm.Checked.Stats.PackagesAnalyzed != 0 {
					t.Fatalf("matching analysis was not reused: %+v", warm.Checked.Stats)
				}
			}
			after, err := json.Marshal(analyzed.Packages["app"].Checked.Program)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("lowering changed published syntax: %v", err)
			}
			request.Target = target.Target{Tags: []string{"different"}}
			retargeted, err := compiler.Prepare(request)
			if err != nil || retargeted.Image == nil || retargeted.Checked.Stats.PackagesAnalyzed == 0 {
				t.Fatalf("target change reused old facts: %v %+v", err, retargeted.Checked)
			}
			request.Target = target.Target{}
			request.Limits = compiler.Limits{MaxASTNodes: 2}
			limited, err := compiler.Prepare(request)
			if err != nil || !source.HasErrors(limited.Checked.Diagnostics) {
				t.Fatalf("analysis bypassed stricter limits: %v %+v", err, limited.Checked)
			}
			request.Limits = compiler.Limits{}
			request.Sources, err = workspace.Overlay(set, []workspace.SourceChange{{ModulePath: "dep", Path: "dep.mgo", Text: "package dep\nthis is invalid"}})
			if err != nil {
				t.Fatal(err)
			}
			changed, err := compiler.Prepare(request)
			if err != nil || !source.HasErrors(changed.Checked.Diagnostics) || changed.Image != nil {
				t.Fatalf("dependency edit reused old analysis: %v %+v", err, changed.Checked)
			}
		})
	}
}
