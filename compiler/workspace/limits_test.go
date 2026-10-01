package workspace

import (
	"fmt"
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
)

func TestSourceGraphCapacityCanExceedDefaultPackageBudget(t *testing.T) {
	packages := make([]SourcePackage, DefaultMaxPackages+1)
	roots := make([]string, len(packages))
	for i := range packages {
		roots[i] = fmt.Sprintf("example/p%d", i)
		packages[i] = SourcePackage{ModulePath: roots[i], Files: []source.File{{Path: "main.mgo", Text: "package sample\n"}}}
	}
	sources, err := NewMemorySourceSet(packages)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, len(packages)} {
		graph, err := LoadHeadersForRootsWithLimits(roots, sources, target.Target{}, Limits{MaxPackages: limit})
		if err != nil {
			t.Fatal(err)
		}
		if limit == 0 {
			if !source.HasErrors(graph.Diagnostics) {
				t.Fatal("expected package budget diagnostic")
			}
		} else if len(graph.Diagnostics) != 0 || len(graph.Packages) != len(packages) {
			t.Fatalf("raised package budget: packages=%d diagnostics=%v", len(graph.Packages), graph.Diagnostics)
		}
	}
}

func TestParsePackageAppliesCombinedASTNodeLimit(t *testing.T) {
	_, diagnostics, err := ParsePackageWithLimits(SourcePackage{
		ModulePath: "example/data",
		Files: []source.File{
			{Path: "a.mgo", Text: "package data\nvar A = 1\n"},
			{Path: "b.mgo", Text: "package data\nvar B = 2\n"},
		},
	}, Limits{MaxASTNodes: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "ast.limit.nodes" {
			return
		}
	}
	t.Fatalf("missing package AST node limit diagnostic: %#v", diagnostics)
}
