package compiler

import (
	"fmt"
	"path"
	"strings"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type workspaceBuildCache struct {
	request Request
}

func (c *workspaceBuildCache) Lookup(pkg workspace.PackageHeader, dependencyExportHashes map[string]string) (packageCacheLookup, error) {
	action, key, err := workspaceCacheAction(pkg, dependencyExportHashes, c.request.Optimization, c.request.Limits)
	if err != nil {
		return packageCacheLookup{}, err
	}
	if c.request.CacheHash {
		material, err := action.MaterialJSON()
		if err != nil {
			return packageCacheLookup{}, err
		}
		c.trace(cache.Event{Kind: "hash", ModulePath: pkg.Source.ModulePath, ActionKey: key, MaterialJSON: material})
	}
	if c.request.Cache == nil {
		return packageCacheLookup{ActionKey: key}, nil
	}
	result, err := c.request.Cache.LookupCompile(action)
	if err != nil {
		c.trace(cache.Event{Kind: "miss", ModulePath: pkg.Source.ModulePath, ActionKey: key, Reason: "cache read failed: " + err.Error()})
		return packageCacheLookup{ActionKey: key}, nil
	}
	if !result.Hit {
		c.trace(cache.Event{Kind: "miss", ModulePath: pkg.Source.ModulePath, ActionKey: key, Reason: result.Reason})
		if !c.request.CacheVerify {
			release, err := c.request.Cache.LockCompile(c.request.Context, action)
			if err != nil {
				return packageCacheLookup{}, err
			}
			retry, retryErr := c.request.Cache.LookupCompile(action)
			if retryErr != nil {
				c.trace(cache.Event{Kind: "miss", ModulePath: pkg.Source.ModulePath, ActionKey: key, Reason: "cache read after lock failed: " + retryErr.Error()})
				return packageCacheLookup{ActionKey: key, Unlock: release}, nil
			}
			if retry.Hit {
				release()
				c.trace(cache.Event{Kind: "shared", ModulePath: pkg.Source.ModulePath, ActionKey: key, ArtifactHash: retry.ArtifactHash, Reason: "reused in-process package build"})
				return packageCacheLookup{
					Artifact: retry.Artifact, Symbols: retry.Symbols, ExportData: retry.ExportData,
					ArtifactHash: retry.ArtifactHash, ExportHash: retry.ExportHash,
					Hit: true, ReuseArtifact: true, ActionKey: key,
				}, nil
			}
			return packageCacheLookup{ActionKey: key, Unlock: release}, nil
		}
		return packageCacheLookup{ActionKey: key}, nil
	}
	c.trace(cache.Event{Kind: "hit", ModulePath: pkg.Source.ModulePath, ActionKey: key, ArtifactHash: result.ArtifactHash, Reason: result.Reason})
	return packageCacheLookup{
		Artifact:      result.Artifact,
		Symbols:       result.Symbols,
		ExportData:    result.ExportData,
		ArtifactHash:  result.ArtifactHash,
		ExportHash:    result.ExportHash,
		Hit:           true,
		ReuseArtifact: !c.request.CacheVerify,
		ActionKey:     key,
	}, nil
}

func (c *workspaceBuildCache) Store(pkg workspace.PackageHeader, dependencyExportHashes map[string]string, artifact ir.Artifact, symbols ir.PackageSymbols, packageData cache.PackageData) error {
	if c.request.Cache == nil {
		return nil
	}
	action, key, err := workspaceCacheAction(pkg, dependencyExportHashes, c.request.Optimization, c.request.Limits)
	if err != nil {
		return err
	}
	manifest, err := c.request.Cache.StoreCompile(action, artifact, symbols, packageData)
	if err != nil {
		c.trace(cache.Event{Kind: "write_error", ModulePath: pkg.Source.ModulePath, ActionKey: key, Reason: err.Error()})
		return nil
	}
	c.trace(cache.Event{Kind: "store", ModulePath: pkg.Source.ModulePath, ActionKey: key, ArtifactHash: manifest.ArtifactHash, ExportHash: manifest.ExportHash, Reason: "stored artifact and export data"})
	return nil
}

func (c *workspaceBuildCache) Verify(pkg workspace.PackageHeader, dependencyExportHashes map[string]string, packageData cache.PackageData, hash, cachedHash, cachedExportHash string) error {
	if hash != cachedHash {
		return fmt.Errorf("compile cache verify failed for %q: rebuilt artifact hash %s, cached %s", pkg.Source.ModulePath, hash, cachedHash)
	}
	if packageData.ExportHash != cachedExportHash {
		return fmt.Errorf("compile cache verify failed for %q: rebuilt export hash %s, cached %s", pkg.Source.ModulePath, packageData.ExportHash, cachedExportHash)
	}
	_, key, err := workspaceCacheAction(pkg, dependencyExportHashes, c.request.Optimization, c.request.Limits)
	if err != nil {
		return err
	}
	c.trace(cache.Event{Kind: "verify", ModulePath: pkg.Source.ModulePath, ActionKey: key, ArtifactHash: hash, ExportHash: packageData.ExportHash, Reason: "rebuilt package state matches cache"})
	return nil
}

func (c *workspaceBuildCache) trace(event cache.Event) {
	if c.request.TraceCache != nil {
		c.request.TraceCache(event)
	}
}

func workspaceCacheAction(pkg workspace.PackageHeader, dependencyExportHashes map[string]string, optimization OptimizationLevel, limits Limits) (cache.Action, string, error) {
	action := cache.NewCompileAction(
		Identity(),
		pkg.Source.SelectionTarget,
		pkg.Source.ModulePath,
		pkg.Package,
		cacheSourceCandidates(pkg.Candidates),
		cacheDependencies(pkg.Imports, dependencyExportHashes),
	)
	action.Optimization = uint8(optimization)
	action.LimitsHash = limits.cacheHash()
	action.PackageID = pkg.Source.ID.String()
	for _, resource := range pkg.Source.Resources {
		action.ResourceFiles = append(action.ResourceFiles, cache.SourceFile{
			Path: cachePath(resource.Path), Hash: resource.Hash, Selected: true,
		})
	}
	key, err := action.Key()
	return action, key, err
}

func cacheSourceCandidates(files []workspace.SourceCandidate) []cache.SourceFile {
	out := make([]cache.SourceFile, 0, len(files))
	for _, file := range files {
		out = append(out, cache.SourceFile{
			Path:     cachePath(file.Path),
			Hash:     file.Hash,
			Selected: file.Selected,
		})
	}
	return out
}

func cacheDependencies(imports []string, dependencyExportHashes map[string]string) []cache.Dependency {
	out := make([]cache.Dependency, 0, len(imports))
	for _, modulePath := range imports {
		if hash := strings.TrimSpace(dependencyExportHashes[modulePath]); hash != "" {
			out = append(out, cache.Dependency{
				ModulePath: strings.TrimSpace(modulePath), ExportHash: hash,
			})
		}
	}
	return out
}

func cachePath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return path.Clean(strings.ReplaceAll(name, "\\", "/"))
}
