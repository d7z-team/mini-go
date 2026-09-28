package cache

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestTransientCacheOwnsValuesAndEvictsAsOneEntry(t *testing.T) {
	store := NewTransient(TransientConfig{MaxEntries: 1, MaxBytes: 1 << 20})
	firstAction := testCacheAction("example/first", "first", nil, nil)
	firstArtifact := ir.NewArtifact("example/first", "first")
	if _, err := store.StoreCompile(firstAction, firstArtifact, mustPackageSymbols(t, firstArtifact), mustPackageData(t, firstArtifact)); err != nil {
		t.Fatal(err)
	}
	lookup, err := store.LookupCompile(firstAction)
	if err != nil || !lookup.Hit {
		t.Fatalf("first lookup = %#v, %v", lookup, err)
	}
	lookup.Artifact.Module.Path = "mutated"
	again, err := store.LookupCompile(firstAction)
	if err != nil || !again.Hit || again.Artifact.Module.Path != "example/first" {
		t.Fatalf("cache shared caller mutation: %#v, %v", again, err)
	}

	secondAction := testCacheAction("example/second", "second", nil, nil)
	secondArtifact := ir.NewArtifact("example/second", "second")
	if _, err := store.StoreCompile(secondAction, secondArtifact, mustPackageSymbols(t, secondArtifact), mustPackageData(t, secondArtifact)); err != nil {
		t.Fatal(err)
	}
	if evicted, err := store.LookupCompile(firstAction); err != nil || evicted.Hit {
		t.Fatalf("evicted lookup = %#v, %v", evicted, err)
	}
	if manifest, err := store.LookupCompileManifest(firstAction); err != nil || manifest.Hit {
		t.Fatalf("evicted manifest = %#v, %v", manifest, err)
	}
	stats := store.Stats()
	if stats.Entries != 1 || stats.Evictions != 1 || stats.Bytes <= 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestTransientEstimatesIncludeVariablePayloads(t *testing.T) {
	text := strings.Repeat("x", 128<<10)
	ref := types.TypeRef{Kind: types.Named, Named: types.TypeKey{ModulePath: text, DeclID: "Value"}}
	cases := map[string]int64{
		"partial declaration": ast.EstimatedDeclBytes(ast.Decl{
			Kind: ast.DeclType,
			Func: ast.FuncDecl{Body: ast.BlockStmt{Stmts: []ast.Statement{{
				Kind: ast.StmtReturn, Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: text}},
			}}}},
		}),
		"generic literal": estimatePackageDataBytes(PackageData{GenericTemplates: []GenericTemplate{{Decl: ast.Decl{
			Kind: ast.DeclFunc, Func: ast.FuncDecl{Body: ast.BlockStmt{Stmts: []ast.Statement{{
				Kind: ast.StmtReturn, Results: []ast.Expression{{Kind: ast.ExprLiteral, Literal: text}},
			}}}},
		}}}}),
		"nested type": estimatePackageDataBytes(PackageData{TypeTable: types.TypeTable{Nodes: []types.TypeNode{{
			Kind: types.Struct, Fields: []types.Field{{Name: "Value", Type: ref, Tag: text}},
		}}}}),
		"entry":      estimateExecutionImageBytes(ir.ExecutionImage{Entries: []ir.Entry{{Name: text}}}),
		"capability": estimateExecutionImageBytes(ir.ExecutionImage{Capabilities: []string{text}}),
		"symbol location": estimatePackageSymbolsBytes(ir.PackageSymbols{Functions: []ir.FunctionSymbols{{
			Locations: []ir.InstructionSymbol{{Points: []ir.Location{{File: text}}}},
		}}}),
	}
	for name, size := range cases {
		if size < int64(len(text)) {
			t.Errorf("%s: estimated %d bytes for %d bytes of content", name, size, len(text))
		}
	}
	const maximum = int64(1<<63 - 1)
	if got := cacheEntryBytes("key", maximum-1, 128); got != maximum {
		t.Fatalf("overflowing estimate = %d", got)
	}
}

func TestTransientPrepareCacheOwnsImage(t *testing.T) {
	action, output := testPreparedOutput(t)
	store := NewTransient(TransientConfig{MaxEntries: 4, MaxBytes: 1 << 20})
	if err := store.StorePrepare(action, output); err != nil {
		t.Fatal(err)
	}
	lookup, err := store.LookupPrepare(action)
	if err != nil || !lookup.Hit {
		t.Fatalf("lookup = %#v, %v", lookup, err)
	}
	archive := lookup.Image.Packages[action.Root]
	archive.Artifact[0] = 'x'
	lookup.Image.Packages[action.Root] = archive
	again, err := store.LookupPrepare(action)
	if err != nil || !again.Hit || again.Image.Packages[action.Root].Artifact[0] == 'x' {
		t.Fatalf("prepare cache shared caller mutation: %#v, %v", again, err)
	}
	store.Clear()
	if stats := store.Stats(); stats.Entries != 0 || stats.Bytes != 0 || stats.Clears != 1 {
		t.Fatalf("cleared stats = %#v", stats)
	}
}

func TestTransientCompileCacheCountsPackageSymbols(t *testing.T) {
	action := testCacheAction("example/symbols", "symbols", nil, nil)
	artifact := ir.NewArtifact("example/symbols", "symbols")
	symbols := mustPackageSymbols(t, artifact)
	symbols.Files = []ir.SourceFile{{ID: "file.large", Path: strings.Repeat("source/", 1024)}}
	data := mustPackageData(t, artifact)
	baseSize := estimateArtifactBytes(artifact) + estimatePackageDataBytes(data)
	symbolSize := estimatePackageSymbolsBytes(symbols)
	store := NewTransient(TransientConfig{MaxEntries: 4, MaxBytes: baseSize + symbolSize - 1})
	if _, err := store.StoreCompile(action, artifact, symbols, data); err != nil {
		t.Fatal(err)
	}
	if lookup, err := store.LookupCompile(action); err != nil || lookup.Hit {
		t.Fatalf("oversized symbol entry remained cached: %#v, %v", lookup, err)
	}
	stats := store.Stats()
	if stats.Entries != 0 || stats.Bytes != 0 || stats.Stores != 0 || stats.Evictions != 0 {
		t.Fatalf("symbol eviction stats = %#v", stats)
	}
}
