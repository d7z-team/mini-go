package compiler

import (
	"context"
	"errors"
	"testing"

	"github.com/d7z-team/mini-go/compiler/lower"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type (
	SourceFile    = source.File
	SourcePackage = workspace.SourcePackage
)

type compileDiagnosticCase struct {
	name   string
	source string
	code   string
}

func functionOperations(t *testing.T, function ir.Function) []ir.Instruction {
	t.Helper()
	operations, err := function.Operations()
	if err != nil {
		t.Fatal(err)
	}
	return operations
}

func (r compiledPackage) EncodeJSON() ([]byte, error) {
	if !r.OK() {
		return nil, errors.New("cannot encode artifact with diagnostics")
	}
	return ir.EncodeJSON(&r.Artifact)
}

func (r compiledPackage) Hash() (string, error) {
	if !r.OK() {
		return "", errors.New("cannot hash artifact with diagnostics")
	}
	return ir.Hash(&r.Artifact)
}

func (r compiledPackage) Disassemble() (string, error) {
	if !r.OK() {
		return "", errors.New("cannot disassemble artifact with diagnostics")
	}
	return ir.Disassemble(&r.Artifact)
}

func compilePackage(pkg SourcePackage) (compiledPackage, error) {
	parsed, diagnostics, err := workspace.ParseOwnedPackageWithLimits(pkg, workspace.Limits{})
	if err != nil {
		return compiledPackage{}, err
	}
	if len(diagnostics) != 0 {
		return compiledPackage{Diagnostics: diagnostics}, nil
	}
	return compileParsedPackageWithLimits(context.Background(), parsed, nil, lower.Options{}, nil, normalizeCompilerLimits(Limits{}), OptimizationDefault)
}

func compileTestSource(modulePath, path, source string) (compiledPackage, error) {
	return compilePackage(SourcePackage{ModulePath: modulePath, Files: []SourceFile{{Path: path, Text: source}}})
}

func compileTestPackage(pkg SourcePackage) (compiledPackage, error) {
	return compilePackage(pkg)
}

func compileTestWorkspace(packages []SourcePackage) (Result, error) {
	sources, err := workspace.NewMemorySourceSet(packages)
	if err != nil {
		return Result{}, err
	}
	root := packages[len(packages)-1].ModulePath
	imported := map[string]struct{}{}
	packageNames := map[string]string{}
	for _, source := range packages {
		parsed, diagnostics, err := workspace.ParsePackage(source)
		if err != nil {
			return Result{}, err
		}
		if len(diagnostics) != 0 {
			return Result{Diagnostics: diagnostics}, nil
		}
		packageNames[source.ModulePath] = parsed.Program.Package
		for _, dependency := range parsed.Imports {
			imported[dependency] = struct{}{}
		}
	}
	var candidates []string
	for _, source := range packages {
		if _, ok := imported[source.ModulePath]; !ok {
			candidates = append(candidates, source.ModulePath)
		}
	}
	if len(candidates) == 1 {
		root = candidates[0]
	} else {
		for _, candidate := range candidates {
			if packageNames[candidate] == "main" {
				root = candidate
				break
			}
		}
	}
	return Compile(Request{Root: root, Sources: sources})
}

func artifactType(artifact ir.Artifact, name string) (types.TypeNode, bool) {
	for _, typ := range artifact.TypeTable.DefinedNamed(artifact.Module.Path) {
		if string(typ.Identity.DeclID) == name {
			return typ, true
		}
	}
	return types.TypeNode{}, false
}

func artifactFunctionByName(artifact ir.Artifact, symbols ir.PackageSymbols, name string) (ir.Function, ir.FunctionSymbols, bool) {
	functionID := ""
	var functionSymbols ir.FunctionSymbols
	for _, symbol := range symbols.Functions {
		if symbol.Name == name {
			functionID, functionSymbols = symbol.ID, symbol
			break
		}
	}
	for _, function := range artifact.Functions {
		if function.ID == functionID {
			return function, functionSymbols, true
		}
	}
	return ir.Function{Code: &ir.SlotCode{}}, ir.FunctionSymbols{}, false
}

func packageGlobalNames(symbols ir.PackageSymbols) map[string]string {
	out := make(map[string]string, len(symbols.Globals))
	for _, global := range symbols.Globals {
		out[global.ID] = global.Name
	}
	return out
}

func functionLocalNames(symbols ir.FunctionSymbols) map[string]string {
	out := make(map[string]string, len(symbols.Locals))
	for _, local := range symbols.Locals {
		out[local.ID] = local.Name
	}
	return out
}

func requireCompileDiagnostic(t *testing.T, diagnostics []source.Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if string(diagnostic.Code) == code {
			return
		}
	}
	t.Fatalf("missing diagnostic %q: %#v", code, diagnostics)
}

func runCompileDiagnosticCases(t *testing.T, modulePath string, tests []compileDiagnosticCase) {
	t.Helper()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := compileTestSource(modulePath, "main.mgo", test.source)
			if err != nil {
				t.Fatal(err)
			}
			if result.OK() {
				t.Fatalf("expected diagnostic %s", test.code)
			}
			requireCompileDiagnostic(t, result.Diagnostics, test.code)
		})
	}
}
