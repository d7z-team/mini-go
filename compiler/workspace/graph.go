package workspace

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
)

const (
	GraphFormat  = "mini-go-workspace-graph"
	GraphVersion = 3
)

type Package struct {
	Source    SourcePackage
	Documents []parser.Document
	Program   ast.Program
	Imports   []string
	Hash      string
}

type Graph struct {
	Root        string
	Target      target.Target
	Packages    map[string]Package
	Order       []string
	Hash        string
	Diagnostics []source.Diagnostic
}

func Load(root string, sources SourceSet, buildTarget target.Target) (Graph, error) {
	return LoadWithLimits(root, sources, buildTarget, Limits{})
}

func LoadWithLimits(root string, sources SourceSet, buildTarget target.Target, limits Limits) (Graph, error) {
	headers, err := LoadHeadersWithLimits(root, sources, buildTarget, limits)
	if err != nil {
		return Graph{}, err
	}
	graph := Graph{Root: headers.Root, Target: headers.Target, Packages: map[string]Package{}, Order: append([]string(nil), headers.Order...), Hash: headers.Hash, Diagnostics: append([]source.Diagnostic(nil), headers.Diagnostics...)}
	if len(graph.Diagnostics) != 0 {
		return graph, nil
	}
	for _, modulePath := range graph.Order {
		parsed, diagnostics, parseErr := ParsePackageWithLimits(headers.Packages[modulePath].Source, limits)
		if parseErr != nil {
			return Graph{}, parseErr
		}
		graph.Diagnostics = append(graph.Diagnostics, diagnostics...)
		graph.Packages[modulePath] = parsed
	}
	graph.Diagnostics = source.NormalizeDiagnostics(graph.Diagnostics)
	return graph, nil
}

func ParsePackage(pkg SourcePackage) (Package, []source.Diagnostic, error) {
	return ParsePackageWithLimits(pkg, Limits{})
}

// OwnedPackage carries finalized syntax until semantic analysis consumes it.
// Source and Imports are metadata; editing them cannot edit the owned syntax.
type OwnedPackage struct {
	Source  SourcePackage
	Syntax  parser.Package
	Imports []string
	Hash    string
}

func ParsePackageWithLimits(pkg SourcePackage, limits Limits) (Package, []source.Diagnostic, error) {
	owned, diagnostics, err := ParseOwnedPackageWithLimits(pkg, limits)
	if err != nil {
		return Package{}, diagnostics, err
	}
	program, documents, _ := owned.Syntax.Take(ast.Limits{})
	return Package{Source: owned.Source, Documents: documents, Program: program, Imports: owned.Imports, Hash: owned.Hash}, diagnostics, nil
}

// ParseOwnedPackageWithLimits binds source resources before finalizing a tree
// that has never been exposed to mutable callers.
func ParseOwnedPackageWithLimits(pkg SourcePackage, limits Limits) (OwnedPackage, []source.Diagnostic, error) {
	limits = normalizeLimits(limits)
	pkg, err := normalizePackage(pkg)
	if err != nil {
		return OwnedPackage{}, nil, err
	}
	for i := range pkg.Files {
		if pkg.Files[i].Hash == "" {
			pkg.Files[i].Hash = source.HashText(pkg.Files[i].Text)
		}
	}
	builder := parser.NewPackageBuilder(pkg.ModulePath, pkg.Files, limits.parserLimits())
	embedDiagnostics := resolveEmbeds(builder, pkg.Resources)
	syntax, imports, diagnostics := builder.Finalize()
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	collector.AddAll(diagnostics...)
	collector.AddAll(embedDiagnostics...)
	return OwnedPackage{Source: pkg, Syntax: syntax, Imports: imports, Hash: packageContentHash(pkg)}, source.NormalizeDiagnostics(collector.Diagnostics()), nil
}

func packageContentHash(pkg SourcePackage) string {
	hasher := sha256.New()
	hasher.Write([]byte(source.HashFiles(pkg.Files)))
	for _, resource := range pkg.Resources {
		hasher.Write([]byte{0})
		hasher.Write([]byte(resource.Path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(resource.Hash))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func workspaceDiagnostic(code, message string) source.Diagnostic {
	return source.Diagnostic{Code: source.DiagnosticCode(code), Severity: source.SeverityError, Message: message}
}
