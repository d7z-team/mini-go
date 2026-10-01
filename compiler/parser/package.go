package parser

import (
	"fmt"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
)

// PackageBuilder owns syntax while files and resolved embed inputs are joined.
// It exposes declaration metadata, never mutable AST storage. Like the parser,
// a builder and the package it produces have one owner at a time.
type PackageBuilder struct{ state *packageSyntax }

type packageSyntax struct {
	program     *ast.Program
	documents   []Document
	limits      ast.Limits
	diagnostics []source.Diagnostic
}

// Package is a finalized package awaiting one transfer to semantic analysis.
// Copies share the transfer state; consuming one invalidates all copies.
type Package struct{ finalized *packageSyntax }

// EmbedDeclaration describes the package variable that owns a directive.
type EmbedDeclaration struct {
	File, Declaration int
	Span              source.Span
	Patterns          []string
	ImportsEmbed      bool
	ValidVariable     bool
}

// NewPackageBuilder parses source files into an owned, unpublished tree. The
// merged structure, embeds and node identities are checked together by Finalize.
func NewPackageBuilder(modulePath string, files []source.File, limits Limits) PackageBuilder {
	limits = normalizeLimits(limits)
	state := &packageSyntax{program: &ast.Program{ModulePath: modulePath}, limits: ast.Limits{
		MaxDepth: limits.MaxNesting, MaxNodes: limits.MaxASTNodes, MaxDiagnostics: limits.MaxDiagnostics,
	}}
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	for _, file := range files {
		if file.OriginPath != "" {
			file.Path = file.OriginPath
		}
		lexical := scanner.ScanDocument(file, limits.Scanner)
		parsed := parseSyntax(modulePath, lexical, limits)
		collector.AddAll(parsed.Diagnostics...)
		document := Document{File: lexical.File(), Diagnostics: append([]source.Diagnostic(nil), parsed.Diagnostics...), lexical: lexical}
		state.documents = append(state.documents, document)
		if state.program.Package == "" {
			state.program.Package, state.program.PackageID = parsed.Program.Package, parsed.Program.PackageID
		} else if parsed.Program.Package != "" && parsed.Program.Package != state.program.Package {
			diagnostic := source.Diagnostic{
				Code: "compiler.package.name.mismatch", Severity: source.SeverityError,
				Message: fmt.Sprintf("file %q has package %q, want %q", file.Path, parsed.Program.Package, state.program.Package),
			}
			if len(parsed.Program.Files) != 0 {
				diagnostic.Primary = parsed.Program.Files[0].Span
			}
			collector.Add(diagnostic)
		}
		for _, syntax := range parsed.Program.Files {
			syntax.ID = file.ID
			if file.Hash != "" {
				syntax.Hash = file.Hash
			}
			state.program.Files = append(state.program.Files, syntax)
		}
	}
	state.diagnostics = collector.Diagnostics()
	return PackageBuilder{state: state}
}

// Embeds returns independent metadata for source resolution by the workspace.
func (b PackageBuilder) Embeds() []EmbedDeclaration {
	var declarations []EmbedDeclaration
	if b.state == nil || b.state.program == nil {
		return declarations
	}
	for i, file := range b.state.program.Files {
		importsEmbed := false
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclImport && decl.Import.Path == "embed" {
				importsEmbed = true
				break
			}
		}
		for j, decl := range file.Decls {
			if decl.Kind == ast.DeclVar && len(decl.Var.EmbedPatterns) != 0 {
				declarations = append(declarations, EmbedDeclaration{
					File: i, Declaration: j, Span: decl.Span,
					Patterns: append([]string(nil), decl.Var.EmbedPatterns...), ImportsEmbed: importsEmbed,
					ValidVariable: len(decl.Var.Names) == 1 && decl.Var.Names[0] != "_" && len(decl.Var.Values) == 0,
				})
			}
		}
	}
	return declarations
}

// BindEmbed copies resolved bytes into the declaration. Caller-owned storage
// and metadata cannot change syntax after finalization.
func (b PackageBuilder) BindEmbed(file, declaration int, files []ast.EmbedFile) bool {
	if b.state == nil || b.state.program == nil || file < 0 || file >= len(b.state.program.Files) || declaration < 0 || declaration >= len(b.state.program.Files[file].Decls) {
		return false
	}
	decl := &b.state.program.Files[file].Decls[declaration]
	if decl.Kind != ast.DeclVar || len(decl.Var.EmbedPatterns) == 0 || len(decl.Var.Names) != 1 || decl.Var.Names[0] == "_" || len(decl.Var.Values) != 0 {
		return false
	}
	owned := make([]ast.EmbedFile, len(files))
	for i, file := range files {
		owned[i] = ast.EmbedFile{Path: file.Path, Data: append([]byte(nil), file.Data...)}
	}
	decl.Var.Values = []ast.Expression{{Kind: ast.ExprEmbed, Span: decl.Span, EmbedFiles: owned}}
	return true
}

// Finalize transfers the builder and validates the whole package exactly once.
// Documents remain private until Take transfers syntax to its consumer.
func (b PackageBuilder) Finalize() (Package, []string, []source.Diagnostic) {
	if b.state == nil || b.state.program == nil {
		diagnostic := source.Diagnostic{Code: "ast.program.missing", Severity: source.SeverityError, Message: "missing package syntax"}
		return Package{}, nil, []source.Diagnostic{diagnostic}
	}
	state := *b.state
	*b.state = packageSyntax{}
	structural, stats := ast.FinalizeStructure(state.program, state.limits)
	collector := source.NewDiagnosticCollector(state.limits.MaxDiagnostics)
	collector.AddAll(state.diagnostics...)
	collector.AddAll(structural...)
	state.diagnostics = collector.Diagnostics()
	for i := range state.diagnostics {
		state.diagnostics[i].ModulePath = state.program.ModulePath
	}
	state.diagnostics = source.NormalizeDiagnostics(state.diagnostics)
	imports := map[string]struct{}{}
	for i, file := range state.program.Files {
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclImport && !decl.Import.IsEmbedMarker() {
				if path := strings.TrimSpace(decl.Import.Path); path != "" {
					imports[path] = struct{}{}
				}
			}
		}
		document := &state.documents[i]
		for _, diagnostic := range structural {
			if diagnostic.Primary.Start.File == file.Path {
				diagnostic.ModulePath = state.program.ModulePath
				document.Diagnostics = append(document.Diagnostics, diagnostic)
			}
		}
		document.Diagnostics = source.NormalizeDiagnostics(document.Diagnostics)
		document.Program = ast.Program{
			NodeID: state.program.NodeID, ModulePath: state.program.ModulePath, Package: file.PackageID.Text,
			PackageID: file.PackageID, Files: []ast.File{file},
		}
		if i < len(stats.FileNodes) {
			document.NodeCount = stats.FileNodes[i] + 1
		}
	}
	ordered := make([]string, 0, len(imports))
	for path := range imports {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return Package{finalized: &state}, ordered, append([]source.Diagnostic(nil), state.diagnostics...)
}

// Take transfers the finalized tree and documents once. Tighter consumer limits
// trigger validation under those limits; external ASTs use semantic.WithOptions.
func (p Package) Take(limits ast.Limits) (ast.Program, []Document, []source.Diagnostic) {
	if p.finalized == nil || p.finalized.program == nil {
		return ast.Program{}, nil, []source.Diagnostic{{Code: "ast.program.missing", Severity: source.SeverityError, Message: "package syntax has already been transferred or is missing"}}
	}
	state := *p.finalized
	*p.finalized = packageSyntax{}
	if limits.MaxDepth > 0 && limits.MaxDepth < state.limits.MaxDepth ||
		limits.MaxNodes > 0 && limits.MaxNodes < state.limits.MaxNodes {
		diagnostics, _ := ast.FinalizeStructure(state.program, limits)
		for i := range diagnostics {
			diagnostics[i].ModulePath = state.program.ModulePath
		}
		state.diagnostics = append(state.diagnostics, diagnostics...)
	}
	if limits.MaxDiagnostics > 0 && limits.MaxDiagnostics < state.limits.MaxDiagnostics {
		collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
		collector.AddAll(state.diagnostics...)
		state.diagnostics = collector.Diagnostics()
	}
	return *state.program, state.documents, state.diagnostics
}

// DiscardDocuments releases lexical records when only compilation is requested.
// It preserves finalized syntax and does not transfer the package.
func (p Package) DiscardDocuments() {
	if p.finalized != nil {
		p.finalized.documents = nil
	}
}
