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
		return packageCacheLookup{Action: action, ActionKey: key}, nil
	}
	result, err := c.request.Cache.LookupCompile(action)
	if err != nil {
		c.trace(cache.Event{Kind: "miss", ModulePath: pkg.Source.ModulePath, ActionKey: key, Reason: "cache read failed: " + err.Error()})
		return packageCacheLookup{Action: action, ActionKey: key}, nil
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
				return packageCacheLookup{Action: action, ActionKey: key, Unlock: release}, nil
			}
			if retry.Hit {
				release()
				c.trace(cache.Event{Kind: "shared", ModulePath: pkg.Source.ModulePath, ActionKey: key, ArtifactHash: retry.ArtifactHash, Reason: "reused in-process package build"})
				return packageCacheLookup{
					Artifact: retry.Artifact, Symbols: retry.Symbols, ExportData: retry.ExportData,
					ArtifactHash: retry.ArtifactHash, ExportHash: retry.ExportHash,
					Hit: true, ReuseArtifact: true, Action: action, ActionKey: key,
				}, nil
			}
			return packageCacheLookup{Action: action, ActionKey: key, Unlock: release}, nil
		}
		return packageCacheLookup{Action: action, ActionKey: key}, nil
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
		Action:        action,
		ActionKey:     key,
	}, nil
}

func (c *workspaceBuildCache) Store(lookup packageCacheLookup, artifact cache.CompiledArtifact, symbols ir.PackageSymbols, packageData cache.PackageData) error {
	if c.request.Cache == nil {
		return nil
	}
	action, key := lookup.Action, lookup.ActionKey
	manifest, err := c.request.Cache.StoreCompile(action, artifact, symbols, packageData)
	if err != nil {
		c.trace(cache.Event{Kind: "write_error", ModulePath: action.ModulePath, ActionKey: key, Reason: err.Error()})
		return nil
	}
	c.trace(cache.Event{Kind: "store", ModulePath: action.ModulePath, ActionKey: key, ArtifactHash: manifest.ArtifactHash, ExportHash: manifest.ExportHash, Reason: "stored artifact and export data"})
	return nil
}

func (c *workspaceBuildCache) Verify(lookup packageCacheLookup, packageData cache.PackageData, hash string) error {
	modulePath := lookup.Action.ModulePath
	if hash != lookup.ArtifactHash {
		return fmt.Errorf("compile cache verify failed for %q: rebuilt artifact hash %s, cached %s", modulePath, hash, lookup.ArtifactHash)
	}
	if packageData.ExportHash != lookup.ExportHash {
		return fmt.Errorf("compile cache verify failed for %q: rebuilt export hash %s, cached %s", modulePath, packageData.ExportHash, lookup.ExportHash)
	}
	c.trace(cache.Event{Kind: "verify", ModulePath: modulePath, ActionKey: lookup.ActionKey, ArtifactHash: hash, ExportHash: packageData.ExportHash, Reason: "rebuilt package state matches cache"})
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
