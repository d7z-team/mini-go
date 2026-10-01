package cache

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func mustSealArtifact(t testing.TB, artifact ir.Artifact) CompiledArtifact {
	t.Helper()
	sealed, err := SealArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func testCacheAction(modulePath, packageName string, sources []SourceFile, dependencies []Dependency) Action {
	return Action{
		Format: Format, Version: Version, Compiler: ir.CompilerIdentity,
		IRFormat: ir.Format, IRVersion: ir.CurrentVersion, OpcodeSet: ir.OpcodeSet, IntrinsicSchema: ir.IntrinsicSchema,
		ModulePath: modulePath, Package: packageName, SourceFiles: sources,
		DependencyHashes: dependencies,
	}
}

func mustPackageData(t testing.TB, artifact ir.Artifact) PackageData {
	t.Helper()
	data, err := FromArtifact(artifact)
	if err != nil {
		t.Fatalf("project export data: %v", err)
	}
	return data
}

func mustPackageSymbols(t testing.TB, artifact ir.Artifact) ir.PackageSymbols {
	t.Helper()
	codeHash, err := ir.Hash(&artifact)
	if err != nil {
		t.Fatalf("hash artifact symbols: %v", err)
	}
	symbols := ir.PackageSymbols{ModulePath: artifact.Module.Path, CodeHash: codeHash}
	for _, global := range artifact.Globals {
		symbols.Globals = append(symbols.Globals, ir.GlobalSymbol{ID: global.ID, Name: global.ID})
	}
	for _, function := range artifact.Functions {
		functionSymbols := ir.FunctionSymbols{ID: function.ID, Name: function.ID}
		for _, local := range function.Locals {
			functionSymbols.Locals = append(functionSymbols.Locals, ir.LocalSymbol{ID: local.ID, Name: local.ID})
		}
		for _, upvalue := range function.Upvalues {
			functionSymbols.Upvalues = append(functionSymbols.Upvalues, ir.UpvalueSymbol{ID: upvalue.ID, Name: upvalue.ID})
		}
		symbols.Functions = append(symbols.Functions, functionSymbols)
	}
	return symbols
}
