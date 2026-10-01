package compiler

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/lower"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type workspaceCompiler struct {
	bindDependencies    bool
	request             Request
	buildCache          *workspaceBuildCache
	graph               *resolvedSourceGraph
	artifacts           map[string]ir.Artifact
	symbols             map[string]ir.PackageSymbols
	exports             map[string]cache.PackageData
	boundArtifactHashes map[string]string
	packageActionHashes map[string]string
	exportHash          map[string]string
	dependencies        dependencyTraversal
	diagnostics         []source.Diagnostic
	stats               Stats
	previousAnalysis    *AnalysisResult
}

func compileWorkspace(request Request, buildCache *workspaceBuildCache, bindDependencies bool) (Result, error) {
	if err := request.Context.Err(); err != nil {
		return Result{}, err
	}
	headerGraph, err := loadHeaderGraph(request, []string{request.Root})
	if err != nil {
		return Result{}, err
	}
	if err := request.Context.Err(); err != nil {
		return Result{}, err
	}
	stats := Stats{PackagesScanned: len(headerGraph.Packages)}
	if len(headerGraph.Diagnostics) != 0 {
		return Result{
			Target: headerGraph.Target, Artifacts: map[string]ir.Artifact{}, GraphHash: headerGraph.Hash,
			Diagnostics: headerGraph.Diagnostics, Stats: stats,
		}, nil
	}
	packages := len(headerGraph.Packages)
	build := &workspaceCompiler{
		request: request, buildCache: buildCache, graph: newResolvedSourceGraph(headerGraph), stats: stats, bindDependencies: bindDependencies,
		artifacts:           make(map[string]ir.Artifact, packages),
		symbols:             make(map[string]ir.PackageSymbols, packages),
		exports:             make(map[string]cache.PackageData, packages),
		boundArtifactHashes: make(map[string]string, packages),
		packageActionHashes: make(map[string]string, packages),
		exportHash:          make(map[string]string, packages),
		dependencies:        newDependencyTraversal(packages),
	}
	if previous := request.PreviousAnalysis; previous != nil && previous.GraphHash == headerGraph.Hash &&
		previous.Target.Equal(headerGraph.Target) && previous.Limits == request.Limits && !source.HasErrors(previous.Diagnostics) {
		build.previousAnalysis = previous
	}
	if err := build.compilePackage(request.Root); err != nil {
		return Result{}, err
	}
	result := Result{
		Target: headerGraph.Target, Artifacts: build.artifacts, PackageSymbols: build.symbols,
		Order: build.dependencies.order, GraphHash: build.graph.hash(request.Root, build.dependencies.order),
		Diagnostics: build.diagnostics, Stats: build.stats,
	}
	if source.HasErrors(build.diagnostics) {
		return result, nil
	}
	result.ArtifactHashes = build.boundArtifactHashes
	result.PackageActionHashes = build.packageActionHashes
	result.ExportData = build.exports
	result.ExportHashes = build.exportHash
	return result, nil
}

func (c *workspaceCompiler) compilePackage(modulePath string) error {
	if err := c.request.Context.Err(); err != nil {
		return err
	}
	visit, cycle := c.dependencies.enter(modulePath)
	if !visit {
		if len(cycle) != 0 {
			c.diagnostics = append(c.diagnostics, diagnostic("compiler.workspace.module.cycle", "module dependency cycle: "+strings.Join(cycle, " -> ")))
		}
		return nil
	}
	added, diagnostics, err := c.graph.loadPackageClosure(c.request, modulePath)
	if err != nil {
		return err
	}
	c.stats.PackagesScanned += added
	c.diagnostics = append(c.diagnostics, diagnostics...)
	pkg, exists := c.graph.packages[modulePath]
	if !exists {
		if len(diagnostics) == 0 {
			c.diagnostics = append(c.diagnostics, diagnostic("compiler.workspace.module.missing", fmt.Sprintf("missing source module %q", modulePath)))
		}
		return nil
	}
	for _, dependency := range pkg.Imports {
		if err := c.compilePackage(dependency); err != nil {
			return err
		}
		if source.HasErrors(c.diagnostics) {
			return nil
		}
	}
	if err := c.request.Context.Err(); err != nil {
		return err
	}

	artifact, symbols, data, _, err := c.loadOrCompilePackage(pkg)
	if err != nil || source.HasErrors(c.diagnostics) {
		return err
	}
	for _, requirement := range artifact.Requirements {
		dependency := strings.TrimSpace(requirement.ModulePath)
		if requirement.Kind != ir.RequirementSource || dependency == "" || c.boundArtifactHashes[dependency] != "" {
			continue
		}
		if err := c.compilePackage(dependency); err != nil {
			return err
		}
		if source.HasErrors(c.diagnostics) {
			return nil
		}
	}
	hash := data.ArtifactHash
	if c.bindDependencies {
		if !finalizeDirectArtifactRequirements(modulePath, &artifact, c.boundArtifactHashes, &c.diagnostics) {
			return nil
		}
		hash, err = ir.HashValidated(&artifact)
		if err != nil {
			return err
		}
		// Binding dependency identities changes no exported declarations.
		data, err = data.BindArtifact(artifact, hash)
		if err != nil {
			return err
		}
	}
	symbols.CodeHash = hash
	c.artifacts[modulePath] = artifact
	c.symbols[modulePath] = symbols
	c.exports[modulePath] = data
	c.boundArtifactHashes[modulePath] = hash
	c.exportHash[modulePath] = data.ExportHash
	c.dependencies.complete(modulePath)
	return nil
}

func (c *workspaceCompiler) loadOrCompilePackage(pkg workspace.PackageHeader) (ir.Artifact, ir.PackageSymbols, cache.PackageData, packageCacheLookup, error) {
	if err := c.request.Context.Err(); err != nil {
		return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, packageCacheLookup{}, err
	}
	cached := packageCacheLookup{}
	var err error
	if c.buildCache != nil {
		cached, err = c.buildCache.Lookup(pkg, c.exportHash)
		if err != nil {
			return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
		}
		if cached.Hit {
			c.stats.PackageCacheHits++
		} else {
			c.stats.PackageCacheMisses++
		}
	}
	if cached.Hit && cached.ReuseArtifact {
		if err := c.request.Context.Err(); err != nil {
			return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
		}
		c.packageActionHashes[pkg.Source.ModulePath] = cached.ArtifactHash
		return cached.Artifact, cached.Symbols, cached.ExportData, cached, nil
	}

	release := cached.Unlock
	if release != nil {
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	var parsed workspace.OwnedPackage
	var previous *check.CheckedProgram
	if c.previousAnalysis != nil {
		if analyzed, ok := c.previousAnalysis.Packages[pkg.Source.ModulePath]; ok && analyzed.Checked.Info != nil {
			previous = &analyzed.Checked
			parsed.Imports = pkg.Imports
		}
	}
	if previous == nil {
		var diagnostics []source.Diagnostic
		parsed, diagnostics, err = workspace.ParseOwnedPackageWithLimits(pkg.Source, c.request.Limits.workspaceLimits())
		c.stats.PackagesParsed++
		if err != nil {
			return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
		}
		if len(diagnostics) != 0 {
			c.diagnostics = append(c.diagnostics, diagnostics...)
			return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, nil
		}
	}
	if err := c.request.Context.Err(); err != nil {
		return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
	}
	if previous == nil {
		c.stats.PackagesAnalyzed++
	}
	compiled, err := compileParsedPackageWithLimits(c.request.Context, parsed, previous, lower.Options{
		Dependencies: dependencyPackages(parsed.Imports, c.artifacts),
	}, c.exports, c.request.Limits, c.request.Optimization)
	if err != nil {
		return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
	}
	if compiled.Artifact.Module.Path != "" {
		c.stats.PackagesLowered++
	}
	c.stats.PackagesCompiled++
	if !compiled.OK() {
		c.diagnostics = append(c.diagnostics, compiled.Diagnostics...)
		return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, nil
	}
	hash := compiled.ExportData.ArtifactHash
	c.packageActionHashes[pkg.Source.ModulePath] = hash
	compiled.Symbols.CodeHash = hash
	if c.buildCache != nil {
		if err := c.request.Context.Err(); err != nil {
			return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
		}
		if cached.Hit {
			if err := c.buildCache.Verify(cached, compiled.ExportData, hash); err != nil {
				return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
			}
		} else if err := c.buildCache.Store(cached, compiled.CacheArtifact, compiled.Symbols, compiled.ExportData); err != nil {
			return ir.Artifact{}, ir.PackageSymbols{}, cache.PackageData{}, cached, err
		}
	}
	if release != nil {
		release()
		release = nil
	}
	return compiled.Artifact, compiled.Symbols, compiled.ExportData, cached, nil
}
