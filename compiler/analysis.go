package compiler

import (
	"context"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/identity"
	"github.com/d7z-team/mini-go/compiler/parser"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

// AnalysisRequest selects a semantic snapshot without emitting bytecode.
type AnalysisRequest struct {
	Request             Request
	Roots               []string
	Previous            *AnalysisResult
	ContinueAfterErrors bool
}

// AnalyzedPackage owns the syntax and semantic facts for one source package.
type AnalyzedPackage struct {
	Checked     check.CheckedProgram
	Documents   []parser.Document
	Diagnostics []source.Diagnostic
}

// AnalysisResult is an immutable snapshot that may be supplied to the next analysis.
type AnalysisResult struct {
	Order            []string
	GraphHash        string
	Target           target.Target
	Limits           Limits
	Packages         map[string]AnalyzedPackage
	ExportHashes     map[string]string
	SourceHashes     map[string]string
	DependencyHashes map[string]string
	Diagnostics      []source.Diagnostic
	Stats            Stats
}

// Analyze checks source packages without lowering or emitting bytecode.
// When Previous is supplied, packages whose source and dependency export hashes
// are unchanged reuse their immutable analysis result.
func Analyze(request AnalysisRequest) (AnalysisResult, error) {
	return analyzePackages(request, true)
}

func analyzePackages(request AnalysisRequest, retainFacts bool) (AnalysisResult, error) {
	input := request.Request
	input.Context = requestContext(input.Context)
	input.Limits = normalizeCompilerLimits(input.Limits)
	ctx := input.Context
	if err := ctx.Err(); err != nil {
		return AnalysisResult{}, err
	}
	roots := append([]string(nil), request.Roots...)
	if len(roots) == 0 {
		roots = []string{input.Root}
	}
	limits := input.Limits.workspaceLimits()
	graph, err := loadHeaderGraph(input, roots)
	if err != nil {
		return AnalysisResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return AnalysisResult{}, err
	}
	result := AnalysisResult{
		Order: append([]string(nil), graph.Order...), GraphHash: graph.Hash,
		Target:           graph.Target,
		Limits:           input.Limits,
		Packages:         make(map[string]AnalyzedPackage, len(graph.Packages)),
		ExportHashes:     make(map[string]string, len(graph.Packages)),
		SourceHashes:     make(map[string]string, len(graph.Packages)),
		DependencyHashes: make(map[string]string, len(graph.Packages)),
		Diagnostics:      append([]source.Diagnostic(nil), graph.Diagnostics...),
		Stats:            Stats{PackagesScanned: len(graph.Packages)},
	}
	if source.HasErrors(result.Diagnostics) {
		return result, nil
	}
	exports := make(map[string][]check.DependencyExport, len(graph.Packages))
	for _, modulePath := range graph.Order {
		if err := ctx.Err(); err != nil {
			return AnalysisResult{}, err
		}
		header := graph.Packages[modulePath]
		var sourceIdentity strings.Builder
		sourceIdentity.WriteString(header.Hash)
		for _, resource := range header.Source.Resources {
			sourceIdentity.WriteByte(0)
			sourceIdentity.WriteString(resource.Path)
			sourceIdentity.WriteByte(0)
			sourceIdentity.WriteString(resource.Hash)
		}
		result.SourceHashes[modulePath] = source.HashText(sourceIdentity.String())
		dependencies := make([]string, 0, len(header.Imports))
		dependencyFacts := semanticDependencies(header.Imports, func(path string) ([]check.DependencyExport, []string, bool) {
			members, ok := exports[path]
			return members, graph.Packages[path].Imports, ok
		})
		for _, dependency := range header.Imports {
			dependencies = append(dependencies, dependency+"="+result.ExportHashes[dependency])
		}
		sort.Strings(dependencies)
		result.DependencyHashes[modulePath] = source.HashText(strings.Join(dependencies, "\n"))
		if previous := request.Previous; previous != nil && previous.Target.Equal(graph.Target) && previous.Limits == input.Limits &&
			previous.SourceHashes[modulePath] == result.SourceHashes[modulePath] &&
			previous.DependencyHashes[modulePath] == result.DependencyHashes[modulePath] {
			if pkg, ok := previous.Packages[modulePath]; ok {
				result.ExportHashes[modulePath] = previous.ExportHashes[modulePath]
				if !source.HasErrors(pkg.Diagnostics) {
					exports[modulePath] = DependencyExports(pkg.Checked.Info)
				}
				if !retainFacts {
					pkg.Checked = check.CheckedProgram{}
				}
				result.Packages[modulePath] = pkg
				result.Diagnostics = append(result.Diagnostics, pkg.Diagnostics...)
				result.Stats.PackageCacheHits++
				if source.HasErrors(pkg.Diagnostics) && !request.ContinueAfterErrors {
					return result, nil
				}
				continue
			}
		}
		parsed, diagnostics, parseErr := workspace.ParseOwnedPackageWithLimits(header.Source, limits)
		result.Stats.PackagesParsed++
		if parseErr != nil {
			return AnalysisResult{}, parseErr
		}
		for i := range diagnostics {
			diagnostics[i].ModulePath = modulePath
		}
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		if source.HasErrors(diagnostics) {
			if !request.ContinueAfterErrors {
				return result, nil
			}
			continue
		}
		sourceChecked, documents, err := analyzeParsedProgram(ctx, parsed.Syntax, dependencyFacts, input.Limits)
		if err != nil {
			return AnalysisResult{}, err
		}
		result.Stats.PackagesAnalyzed++
		pkg := AnalyzedPackage{Checked: sourceChecked, Documents: documents, Diagnostics: append([]source.Diagnostic(nil), sourceChecked.Info.Diagnostics...)}
		result.Diagnostics = append(result.Diagnostics, pkg.Diagnostics...)
		if !source.HasErrors(pkg.Diagnostics) {
			exports[modulePath] = DependencyExports(sourceChecked.Info)
		}
		// The public surface also depends on the definitions of named types
		// referenced through imports, even when our own signatures are unchanged.
		var dependencyTypes []check.DependencyPackage
		for _, dependency := range dependencyFacts {
			pkg := check.DependencyPackage{ModulePath: dependency.ModulePath}
			for _, member := range dependency.Members {
				if member.Kind == check.ObjectType {
					pkg.Members = append(pkg.Members, member)
				}
			}
			if len(pkg.Members) != 0 {
				dependencyTypes = append(dependencyTypes, pkg)
			}
		}
		identityWriter := identity.New("mini-go/analysis-exports/binary/v1")
		identityWriter.Bool(exports[modulePath] != nil)
		identityWriter.Uint(uint64(len(exports[modulePath])))
		for _, member := range exports[modulePath] {
			check.EncodeIdentityDependencyExport(identityWriter, member)
		}
		identityWriter.Bool(dependencyTypes != nil)
		identityWriter.Uint(uint64(len(dependencyTypes)))
		for _, dependency := range dependencyTypes {
			check.EncodeIdentityDependencyPackage(identityWriter, dependency)
		}
		digest, encodeErr := identityWriter.Sum()
		if encodeErr != nil {
			return AnalysisResult{}, encodeErr
		}
		result.ExportHashes[modulePath] = hex.EncodeToString(digest[:])
		result.Stats.PackageCacheMisses++
		if !retainFacts {
			pkg.Checked = check.CheckedProgram{}
		}
		result.Packages[modulePath] = pkg
		if source.HasErrors(pkg.Diagnostics) && !request.ContinueAfterErrors {
			return result, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return AnalysisResult{}, err
	}
	result.Diagnostics = source.NormalizeDiagnostics(result.Diagnostics)
	return result, nil
}

func analyzeProgram(ctx context.Context, program ast.Program, dependencies []check.DependencyPackage, limits Limits) (check.CheckedProgram, error) {
	if err := ctx.Err(); err != nil {
		return check.CheckedProgram{}, err
	}
	checked := check.WithOptions(program, check.AnalyzeOptions{Dependencies: dependencies, Limits: ast.Limits{
		MaxDepth: limits.MaxSyntaxDepth, MaxNodes: limits.MaxASTNodes, MaxDiagnostics: limits.MaxDiagnostics,
	}})
	if err := ctx.Err(); err != nil {
		return check.CheckedProgram{}, err
	}
	checked.Info.Diagnostics = boundedDiagnostics(checked.Info.Diagnostics, limits.MaxDiagnostics)
	return checked, nil
}

func analyzeParsedProgram(ctx context.Context, input parser.Package, dependencies []check.DependencyPackage, limits Limits) (check.CheckedProgram, []parser.Document, error) {
	if err := ctx.Err(); err != nil {
		return check.CheckedProgram{}, nil, err
	}
	checked, documents := check.WithParsed(input, check.AnalyzeOptions{Dependencies: dependencies, Limits: ast.Limits{
		MaxDepth: limits.MaxSyntaxDepth, MaxNodes: limits.MaxASTNodes, MaxDiagnostics: limits.MaxDiagnostics,
	}})
	if err := ctx.Err(); err != nil {
		return check.CheckedProgram{}, nil, err
	}
	checked.Info.Diagnostics = boundedDiagnostics(checked.Info.Diagnostics, limits.MaxDiagnostics)
	return checked, documents, nil
}
