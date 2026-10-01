package compiler

import (
	"context"
	"testing"

	"github.com/d7z-team/mini-go/compiler/lower"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileOptimizationLevelsPreserveExportsAndSymbols(t *testing.T) {
	input := SourcePackage{
		ModulePath: "example/main",
		Files: []SourceFile{{Path: "main.mgo", Text: `package main
func target() int { return 1 }
func Wrapper() int {
	value := target()
	if value > 0 {
		inner := value + 1
		return inner
	}
	return value
}
`}},
	}
	results := make([]compiledPackage, 3)
	for level := OptimizationNone; level <= OptimizationFull; level++ {
		parsed, diagnostics, err := workspace.ParseOwnedPackageWithLimits(input, workspace.Limits{})
		if err != nil || len(diagnostics) != 0 {
			t.Fatalf("parse: diagnostics=%#v err=%v", diagnostics, err)
		}
		results[level], err = compileParsedPackageWithLimits(context.Background(), parsed, nil, lower.Options{}, nil, normalizeCompilerLimits(Limits{}), level)
		if err != nil || !results[level].OK() {
			t.Fatalf("compile O%d: diagnostics=%#v err=%v", level, results[level].Diagnostics, err)
		}
		if err := ir.ValidateArtifact(&results[level].Artifact); err != nil {
			t.Fatalf("validate O%d: %v", level, err)
		}
	}
	for level := OptimizationDefault; level <= OptimizationFull; level++ {
		if results[level].ExportData.ExportHash != results[OptimizationNone].ExportData.ExportHash {
			t.Fatalf("O%d changed export hash: %s != %s", level, results[level].ExportData.ExportHash, results[OptimizationNone].ExportData.ExportHash)
		}
	}
	for level := OptimizationNone; level <= OptimizationFull; level++ {
		wrapper, wrapperSymbols, ok := artifactFunctionByName(results[level].Artifact, results[level].Symbols, "Wrapper")
		if !ok || wrapperSymbols.Declaration == nil || len(wrapperSymbols.Scopes) < 2 {
			t.Fatalf("O%d wrapper symbol metadata = %#v", level, wrapperSymbols)
		}
		foundScopedLocal := false
		for _, local := range wrapperSymbols.Locals {
			if local.Name == "inner" && local.Scope != 0 && local.Declaration != nil {
				foundScopedLocal = true
			}
		}
		if !foundScopedLocal {
			t.Fatalf("O%d nested local metadata = %#v; code=%#v", level, wrapperSymbols.Locals, wrapper.Locals)
		}
	}
}
