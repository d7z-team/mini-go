package main

import (
	"fmt"

	"github.com/d7z-team/mini-go/compiler/analysis"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/stdlib"
)

func buildMiniGoSnapshot(baseline snapshot) (snapshot, error) {
	library := stdlib.Open()
	sources, err := workspace.StandardLibrary(library)
	if err != nil {
		return snapshot{}, err
	}
	available := make(map[string]bool, len(baseline.Packages))
	roots := make([]string, 0, len(baseline.Packages))
	for _, expected := range baseline.Packages {
		if _, found, err := sources.Package(expected.Path); err != nil {
			return snapshot{}, err
		} else if found {
			available[expected.Path] = true
			roots = append(roots, expected.Path)
		}
	}
	result, err := analysis.CheckWorkspace(analysis.WorkspaceRequest{Roots: roots, Sources: sources})
	if err != nil {
		return snapshot{}, fmt.Errorf("compile Mini-Go API graph: %w", err)
	}
	if source.HasErrors(result.Diagnostics) {
		return snapshot{}, fmt.Errorf("compile Mini-Go API graph: %v", result.Diagnostics)
	}
	out := snapshot{Schema: schema, Version: version, GoVersion: goVersion}
	for _, expected := range baseline.Packages {
		if !available[expected.Path] {
			continue
		}
		checked, ok := result.Packages[expected.Path]
		if !ok {
			return snapshot{}, fmt.Errorf("Mini-Go package %s has no semantic analysis", expected.Path)
		}
		projected := analysis.ProjectPublicAPI(checked)
		entry := apiPackage{Path: expected.Path, Complete: expected.Complete}
		for _, declaration := range projected.Declarations {
			if !trackedDeclaration(expected.Path, declaration.Kind, declaration.Name) {
				continue
			}
			decl := apiDeclaration{Kind: declaration.Kind, Name: declaration.Name, Type: declaration.Type, Alias: declaration.Alias, Untyped: declaration.Untyped, Exact: declaration.Exact}
			decl.Fields = append(decl.Fields, declaration.Fields...)
			for _, method := range declaration.Methods {
				if isOperatorMethod(method.Name) {
					continue
				}
				decl.Methods = append(decl.Methods, method)
			}
			decl.TypeParams = append(decl.TypeParams, declaration.TypeParams...)
			entry.Declarations = append(entry.Declarations, decl)
		}
		out.Packages = append(out.Packages, entry)
	}
	return out, nil
}

func isOperatorMethod(name string) bool {
	switch name {
	case "OpAdd", "OpSub", "OpMul", "OpDiv", "OpRem", "OpAnd", "OpOr", "OpXor", "OpShl", "OpShr", "OpAndNot",
		"OpNeg", "OpPos", "OpNot", "OpBitNot", "OpEq", "OpNeq", "OpLt", "OpLte", "OpGt", "OpGte":
		return true
	default:
		return false
	}
}
