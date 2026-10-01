package compiler

import (
	"context"
	"strings"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/emit"
	"github.com/d7z-team/mini-go/compiler/lower"
	"github.com/d7z-team/mini-go/compiler/optimize"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/specialize"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/types"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type Result struct {
	Target         target.Target
	Artifacts      map[string]ir.Artifact
	PackageSymbols map[string]ir.PackageSymbols
	// ArtifactHashes identify artifacts with their dependency hashes bound.
	ArtifactHashes map[string]string
	// PackageActionHashes identify unbound package artifacts stored by compile actions.
	PackageActionHashes map[string]string
	ExportData          map[string]cache.PackageData
	ExportHashes        map[string]string
	Order               []string
	GraphHash           string
	Diagnostics         []source.Diagnostic
	Stats               Stats
}

type Stats struct {
	PackagesScanned    int
	PackagesParsed     int
	PackagesAnalyzed   int
	PackagesLowered    int
	PackagesCompiled   int
	PackageCacheHits   int
	PackageCacheMisses int
	PrepareCacheHits   int
	PrepareCacheMisses int
	ImagesLinked       int
	TestsDiscovered    int
}

type Request struct {
	Context          context.Context
	Root             string
	Target           target.Target
	Sources          workspace.SourceSet
	EntryPoints      []EntryPoint
	Cache            cache.Cache
	CacheVerify      bool
	CacheHash        bool
	TraceCache       func(cache.Event)
	Limits           Limits
	HostCapabilities map[string][]string
	Optimization     OptimizationLevel
	Symbols          bool
	PreviousAnalysis *AnalysisResult
	workspace        *workspace.Loader
}

func loadHeaderGraph(request Request, roots []string) (workspace.HeaderGraph, error) {
	limits := request.Limits.workspaceLimits()
	if request.workspace != nil {
		return request.workspace.LoadHeadersForRoots(roots)
	}
	return workspace.LoadHeadersForRootsWithLimits(roots, request.Sources, request.Target, limits)
}

func (r Result) OK() bool {
	return !source.HasErrors(r.Diagnostics)
}

func (r Result) Artifact(modulePath string) (ir.Artifact, bool) {
	if r.Artifacts == nil {
		return ir.Artifact{}, false
	}
	artifact, ok := r.Artifacts[strings.TrimSpace(modulePath)]
	return artifact, ok
}

type compiledPackage struct {
	CacheArtifact cache.CompiledArtifact
	Artifact      ir.Artifact
	Symbols       ir.PackageSymbols
	Checked       check.CheckedProgram
	ExportData    cache.PackageData
	Diagnostics   []source.Diagnostic
}

func (r compiledPackage) OK() bool {
	return !source.HasErrors(r.Diagnostics)
}

func compileParsedPackageWithLimits(ctx context.Context, parsed workspace.OwnedPackage, previous *check.CheckedProgram, options lower.Options, dependencies map[string]cache.PackageData, limits Limits, optimization OptimizationLevel) (compiledPackage, error) {
	if previous == nil {
		parsed.Syntax.DiscardDocuments()
	}
	packageDependencies := make(map[string]cache.PackageData)
	pending := append([]string(nil), parsed.Imports...)
	for next := 0; next < len(pending); next++ {
		path := pending[next]
		if _, seen := packageDependencies[path]; seen {
			continue
		}
		data, ok := dependencies[path]
		if !ok {
			continue
		}
		packageDependencies[path] = data
		for _, requirement := range data.Requirements {
			if requirement.Kind == ir.RequirementSource {
				pending = append(pending, requirement.ModulePath)
			}
		}
	}
	// Runtime exports contain concrete declarations. Source analysis also needs
	// the signatures of generic templates, including file-scoped dot imports.
	options.Dependencies = append([]check.DependencyPackage(nil), options.Dependencies...)
	for i := range options.Dependencies {
		dependency := &options.Dependencies[i]
		dependency.Members = append([]check.DependencyExport(nil), dependency.Members...)
		path := dependency.ModulePath
		data := dependencies[path]
		for _, template := range data.GenericTemplates {
			switch template.Kind {
			case "function":
				params := make([]string, len(template.Decl.Func.TypeParams))
				for i, param := range template.Decl.Func.TypeParams {
					params[i] = param.Name
				}
				dependency.Members = append(dependency.Members, check.DependencyExport{
					ModulePath: path, Name: template.Name, Kind: check.ObjectFunc, Type: template.Type, TypeParams: params,
				})
			case "type":
				export := check.DependencyExport{ModulePath: path, Name: template.Name, Kind: check.ObjectType, Type: template.Type}
				for _, param := range template.Decl.Type.TypeParams {
					export.TypeParams = append(export.TypeParams, param.Name)
				}
				if node, ok := data.TypeTable.Named(types.TypeKey{ModulePath: path, DeclID: types.DeclID(template.Name)}); ok {
					export.Underlying = types.FormatWithTable(&data.TypeTable, node.Underlying)
				}
				dependency.Members = append(dependency.Members, export)
			}
		}
	}
	var sourceChecked check.CheckedProgram
	var err error
	if previous != nil {
		sourceChecked = *previous
	} else {
		sourceChecked, _, err = analyzeParsedProgram(ctx, parsed.Syntax, options.Dependencies, limits)
	}
	if err != nil {
		return compiledPackage{}, err
	}
	sourceProgram := sourceChecked.Program
	semanticDiagnostics := sourceChecked.Info.Diagnostics
	if len(semanticDiagnostics) != 0 {
		return compiledPackage{Checked: sourceChecked, Diagnostics: boundedDiagnostics(semanticDiagnostics, limits.MaxDiagnostics)}, nil
	}
	if err := ctx.Err(); err != nil {
		return compiledPackage{}, err
	}
	checked := sourceChecked
	if specialize.Required(sourceChecked, packageDependencies) {
		specialized, genericDiagnostics, err := specialize.ApplyWithLimits(sourceChecked, packageDependencies, specialize.Limits{
			MaxSpecializations: limits.MaxSpecializations, MaxDiagnostics: limits.MaxDiagnostics,
		})
		if err != nil {
			return compiledPackage{}, err
		}
		if len(genericDiagnostics) != 0 {
			return compiledPackage{Diagnostics: genericDiagnostics}, nil
		}
		if err := ctx.Err(); err != nil {
			return compiledPackage{}, err
		}
		checked, err = analyzeProgram(ctx, specialized, options.Dependencies, limits)
		if err != nil {
			return compiledPackage{}, err
		}
	}
	hirProgram, diagnostics := lower.Lower(checked, options)
	if len(diagnostics) != 0 {
		return compiledPackage{Diagnostics: boundedDiagnostics(diagnostics, limits.MaxDiagnostics)}, nil
	}
	if err := ctx.Err(); err != nil {
		return compiledPackage{}, err
	}
	hirProgram, err = optimize.Apply(hirProgram, optimize.Level(optimization))
	if err != nil {
		return compiledPackage{Diagnostics: []source.Diagnostic{diagnostic("compiler.optimize", "optimize module "+sourceProgram.ModulePath+": "+err.Error())}}, nil
	}
	if err := ctx.Err(); err != nil {
		return compiledPackage{}, err
	}
	artifact, symbols, err := emit.LowerUnvalidatedWithSymbols(hirProgram)
	if err != nil {
		return compiledPackage{Diagnostics: []source.Diagnostic{diagnostic("compiler.emit", "lower module "+sourceProgram.ModulePath+": "+err.Error())}}, nil
	}
	sealed, err := cache.SealArtifact(artifact)
	if err != nil {
		return compiledPackage{Diagnostics: []source.Diagnostic{diagnostic("compiler.emit", "validate module "+sourceProgram.ModulePath+": "+err.Error())}}, nil
	}
	if err := ctx.Err(); err != nil {
		return compiledPackage{}, err
	}
	packageData, err := buildPackageData(sourceProgram, artifact, sealed.Hash(), symbols, sourceChecked.Info)
	if err != nil {
		return compiledPackage{}, err
	}
	return compiledPackage{Artifact: artifact, CacheArtifact: sealed, Symbols: symbols, Checked: sourceChecked, ExportData: packageData}, nil
}

type packageCacheLookup struct {
	Artifact      ir.Artifact
	Symbols       ir.PackageSymbols
	ExportData    cache.PackageData
	ArtifactHash  string
	ExportHash    string
	Hit           bool
	ReuseArtifact bool
	Action        cache.Action
	ActionKey     string
	Unlock        func()
}

func Compile(request Request) (Result, error) {
	return compile(request, true)
}

// Prepare keeps action artifacts unbound until reachable code has been pruned.
// Public Compile still returns fully bound, content-addressed artifacts.
func compile(request Request, bindDependencies bool) (Result, error) {
	request.Context = requestContext(request.Context)
	if err := request.Context.Err(); err != nil {
		return Result{}, err
	}
	request.Limits = normalizeCompilerLimits(request.Limits)
	if err := validateOptimizationLevel(request.Optimization); err != nil {
		return Result{}, err
	}
	normalizedTarget, err := target.Normalize(request.Target)
	if err != nil {
		return Result{}, err
	}
	request.Target = normalizedTarget
	// Built-in caches validate artifacts before exposing a hit. Adapt custom
	// caches at the public boundary before reusing their structured results.
	switch request.Cache.(type) {
	case nil, cache.Store, *cache.Store, *cache.TransientCache, *sessionCache:
	default:
		session := newSessionCache(request.Cache, cache.TransientConfig{})
		defer session.Close()
		request.Cache = session
	}
	if request.Cache == nil && !request.CacheHash {
		return compileWorkspace(request, nil, bindDependencies)
	}
	return compileWorkspace(request, &workspaceBuildCache{request: request}, bindDependencies)
}

func requestContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func diagnostic(code, message string) source.Diagnostic {
	return source.Diagnostic{
		Code:     source.DiagnosticCode(code),
		Severity: "error",
		Message:  message,
	}
}
