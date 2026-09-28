package compiler

import "github.com/d7z-team/mini-go/compiler/cache"

type prepareManifestResolver struct {
	request      Request
	graph        *resolvedSourceGraph
	stats        *Stats
	exportHashes map[string]string
	manifests    map[string]cache.Manifest
	dependencies dependencyTraversal
	miss         bool
}

func newPrepareManifestResolver(request Request, graph *resolvedSourceGraph, stats *Stats) *prepareManifestResolver {
	packages := len(graph.packages)
	return &prepareManifestResolver{
		request: request, graph: graph, stats: stats,
		exportHashes: make(map[string]string, packages),
		manifests:    make(map[string]cache.Manifest, packages),
		dependencies: newDependencyTraversal(packages),
	}
}

func (r *prepareManifestResolver) load(modulePath string) error {
	if r.miss {
		return nil
	}
	visit, cycle := r.dependencies.enter(modulePath)
	if !visit {
		if len(cycle) != 0 {
			r.miss = true
		}
		return nil
	}
	pkg, exists := r.graph.packages[modulePath]
	if !exists {
		added, diagnostics, err := r.graph.loadPackageClosure(r.request, modulePath)
		if err != nil {
			return err
		}
		if len(diagnostics) != 0 {
			r.miss = true
			return nil
		}
		r.stats.PackagesScanned += added
		pkg, exists = r.graph.packages[modulePath]
		if !exists {
			r.miss = true
			return nil
		}
	}
	for _, dependency := range pkg.Imports {
		if err := r.load(dependency); err != nil {
			return err
		}
	}
	if r.miss {
		return nil
	}
	action, key, err := workspaceCacheAction(pkg, r.exportHashes, r.request.Optimization, r.request.Limits)
	if err != nil {
		return err
	}
	cached, err := r.request.Cache.LookupCompileManifest(action)
	if err != nil {
		tracePrepareCache(r.request, cache.Event{Kind: "package_manifest_miss", ModulePath: modulePath, ActionKey: key, Reason: "cache read failed: " + err.Error()})
		r.miss = true
		return nil
	}
	if !cached.Hit {
		tracePrepareCache(r.request, cache.Event{Kind: "package_manifest_miss", ModulePath: modulePath, ActionKey: key, Reason: cached.Reason})
		r.miss = true
		return nil
	}
	r.stats.PackageCacheHits++
	r.exportHashes[modulePath] = cached.ExportHash
	r.manifests[modulePath] = cached.Manifest
	tracePrepareCache(r.request, cache.Event{Kind: "package_manifest_hit", ModulePath: modulePath, ActionKey: key, ArtifactHash: cached.ArtifactHash, ExportHash: cached.ExportHash, Reason: cached.Reason})
	for _, dependency := range cached.RuntimeDependencies {
		if err := r.load(dependency); err != nil {
			return err
		}
	}
	if r.miss {
		return nil
	}
	r.dependencies.complete(modulePath)
	return nil
}
