package compiler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func lookupPrepared(request Request, mode string) (PrepareResult, bool, error) {
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, false, err
	}
	graph, err := loadHeaderGraph(request, []string{request.Root})
	if err != nil {
		return PrepareResult{}, false, err
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, false, err
	}
	stats := Stats{PackagesScanned: len(graph.Packages)}
	checked := CheckResult{
		Target: graph.Target, Order: append([]string(nil), graph.Order...), GraphHash: graph.Hash,
		Diagnostics: graph.Diagnostics, Stats: stats,
	}
	if len(graph.Diagnostics) != 0 {
		return PrepareResult{Checked: checked}, true, nil
	}
	entries, diagnostic := prepareEntries(request, graph.Packages[request.Root].Package)
	if diagnostic != nil {
		checked.Diagnostics = append(checked.Diagnostics, *diagnostic)
		return PrepareResult{Checked: checked}, true, nil
	}
	resolved := newResolvedSourceGraph(graph)
	manifests := newPrepareManifestResolver(request, resolved, &stats)
	if err := manifests.load(request.Root); err != nil {
		return PrepareResult{}, false, err
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, false, err
	}
	if manifests.miss {
		return PrepareResult{}, false, nil
	}
	graphHash := resolved.hash(request.Root, manifests.dependencies.order)
	checked.Order = append([]string(nil), manifests.dependencies.order...)
	checked.GraphHash = graphHash
	artifacts := make([]cache.PrepareArtifact, 0, len(manifests.dependencies.order))
	for _, modulePath := range manifests.dependencies.order {
		artifacts = append(artifacts, cache.PrepareArtifact{ModulePath: modulePath, Hash: manifests.manifests[modulePath].ArtifactHash})
	}
	checked.ExportHashes = manifests.exportHashes
	action, actionKey, err := newPrepareCacheAction(request, graph.Target, mode, entries, artifacts)
	if err != nil {
		return PrepareResult{}, false, err
	}
	if request.CacheHash {
		material, err := action.MaterialJSON()
		if err != nil {
			return PrepareResult{}, false, err
		}
		tracePrepareCache(request, cache.Event{Kind: "prepare_hash", ModulePath: request.Root, ActionKey: actionKey, MaterialJSON: material})
	}
	cached, err := request.Cache.LookupPrepare(action)
	if err != nil {
		tracePrepareCache(request, cache.Event{Kind: "prepare_miss", ModulePath: request.Root, ActionKey: actionKey, Reason: "cache read failed: " + err.Error()})
		return PrepareResult{}, false, nil
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, false, err
	}
	if !cached.Hit {
		tracePrepareCache(request, cache.Event{Kind: "prepare_miss", ModulePath: request.Root, ActionKey: actionKey, Reason: cached.Reason})
		return PrepareResult{}, false, nil
	}
	checked.Stats = stats
	checked.Stats.PrepareCacheHits = 1
	checked.Stats.TestsDiscovered = len(cached.TestManifest)
	image := cached.Image
	var symbols *ir.ProgramSymbols
	if request.Symbols {
		symbolAction := cache.NewSymbolAction(Identity(), image.Hash, graphHash, uint8(request.Optimization))
		cachedSymbols, symbolErr := request.Cache.LookupSymbols(symbolAction)
		if symbolErr != nil || !cachedSymbols.Hit || ir.ValidateProgramSymbols(&image, &cachedSymbols.Symbols) != nil {
			return PrepareResult{}, false, nil
		}
		owned := cachedSymbols.Symbols
		symbols = &owned
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, false, err
	}
	tracePrepareCache(request, cache.Event{Kind: "prepare_hit", ModulePath: request.Root, ActionKey: actionKey, ArtifactHash: image.Hash, Reason: cached.Reason})
	return PrepareResult{Checked: checked, Image: &image, Symbols: symbols, TestManifest: append([]cache.TestEntry(nil), cached.TestManifest...)}, true, nil
}

func prepareBuiltResult(request Request, built Result, mode string, manifest []cache.TestEntry) (PrepareResult, error) {
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, err
	}
	result := PrepareResult{Checked: CheckResult{
		Target:       built.Target,
		ExportHashes: built.ExportHashes,
		Order:        built.Order, GraphHash: built.GraphHash,
		Diagnostics: built.Diagnostics, Stats: built.Stats,
	}}
	if source.HasErrors(built.Diagnostics) {
		return result, nil
	}
	rootPackage := ""
	if root, ok := built.Artifact(request.Root); ok {
		rootPackage = root.Module.Package
	}
	entries, diagnostic := prepareEntries(request, rootPackage)
	if diagnostic != nil {
		result.Checked.Diagnostics = append(result.Checked.Diagnostics, *diagnostic)
		return result, nil
	}
	modules, err := retainReachableModules(request.Context, built.Artifacts, request.Root)
	if err != nil {
		return PrepareResult{}, err
	}
	artifacts := make([]cache.PrepareArtifact, 0, len(modules))
	for _, modulePath := range built.Order {
		if _, retained := modules[modulePath]; !retained {
			continue
		}
		hash := built.PackageActionHashes[modulePath]
		if hash == "" {
			return PrepareResult{}, fmt.Errorf("package %q has no action artifact hash", modulePath)
		}
		artifacts = append(artifacts, cache.PrepareArtifact{ModulePath: modulePath, Hash: hash})
	}
	prepareAction, actionKey, err := newPrepareCacheAction(request, built.Target, mode, entries, artifacts)
	if err != nil {
		return PrepareResult{}, err
	}
	if request.CacheHash {
		material, err := prepareAction.MaterialJSON()
		if err != nil {
			return PrepareResult{}, err
		}
		tracePrepareCache(request, cache.Event{Kind: "prepare_hash", ModulePath: request.Root, ActionKey: actionKey, MaterialJSON: material})
	}
	var cached cache.PrepareLookup
	var release func()
	if request.Cache != nil {
		cached, err = request.Cache.LookupPrepare(prepareAction)
		if err == nil && cached.Hit {
			result.Checked.Stats.PrepareCacheHits++
			tracePrepareCache(request, cache.Event{Kind: "prepare_hit", ModulePath: request.Root, ActionKey: actionKey, ArtifactHash: cached.Image.Hash, Reason: cached.Reason})
		} else {
			result.Checked.Stats.PrepareCacheMisses++
			reason := cached.Reason
			if err != nil {
				reason = "cache read failed: " + err.Error()
			}
			tracePrepareCache(request, cache.Event{Kind: "prepare_miss", ModulePath: request.Root, ActionKey: actionKey, Reason: reason})
			release, err = request.Cache.LockPrepare(request.Context, prepareAction)
			if err != nil {
				return PrepareResult{}, err
			}
			retry, retryErr := request.Cache.LookupPrepare(prepareAction)
			if retryErr == nil && retry.Hit {
				release()
				release = nil
				result.Checked.Stats.PrepareCacheMisses--
				result.Checked.Stats.PrepareCacheHits++
				tracePrepareCache(request, cache.Event{Kind: "prepare_shared", ModulePath: request.Root, ActionKey: actionKey, ArtifactHash: retry.Image.Hash, Reason: "reused in-process prepare action"})
				cached = retry
			}
		}
		if cached.Hit && !request.CacheVerify {
			if err := request.Context.Err(); err != nil {
				return PrepareResult{}, err
			}
			image := cached.Image
			result.Image = &image
			result.TestManifest = append([]cache.TestEntry(nil), cached.TestManifest...)
			if request.Symbols {
				symbols, symbolErr := loadPreparedSymbols(request, built, image, actionKey)
				if symbolErr != nil {
					return PrepareResult{}, symbolErr
				}
				result.Symbols = symbols
			}
			return result, nil
		}
	}
	if release != nil {
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	image, symbols, err := linkProgram(linkRequest{
		Context:    request.Context,
		CompilerID: Identity(), ContractID: ir.ExecutionContract,
		Target: built.Target,
		Root:   request.Root, Entries: entries,
		Artifacts: built.Artifacts, Symbols: built.PackageSymbols, Order: built.Order,
		HostCapabilities: request.HostCapabilities,
		Optimization:     request.Optimization, IncludeSymbols: request.Symbols,
	})
	if err != nil {
		return PrepareResult{}, err
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, err
	}
	result.Checked.Stats.ImagesLinked++
	result.Image = &image
	result.Symbols = symbols
	result.TestManifest = append([]cache.TestEntry(nil), manifest...)
	if request.Cache != nil {
		if err := request.Context.Err(); err != nil {
			return PrepareResult{}, err
		}
		output := cache.PreparedOutput{Image: image, TestManifest: append([]cache.TestEntry(nil), manifest...)}
		var symbolAction cache.SymbolAction
		if symbols != nil {
			if err := request.Context.Err(); err != nil {
				return PrepareResult{}, err
			}
			symbolAction = symbolActionForBuilt(request, image.Hash, built)
		}
		if cached.Hit && request.CacheVerify {
			cachedJSON, cachedErr := json.Marshal(cache.PreparedOutput{Image: cached.Image, TestManifest: cached.TestManifest})
			outputJSON, outputErr := json.Marshal(output)
			if cachedErr != nil || outputErr != nil || !bytes.Equal(cachedJSON, outputJSON) {
				return PrepareResult{}, fmt.Errorf("prepare cache verify failed for %q", request.Root)
			}
			if symbols != nil {
				cachedSymbols, lookupErr := request.Cache.LookupSymbols(symbolAction)
				if lookupErr != nil {
					return PrepareResult{}, fmt.Errorf("symbol cache verify failed for %q: %w", request.Root, lookupErr)
				}
				if cachedSymbols.Hit {
					if ir.ValidateProgramSymbols(&image, &cachedSymbols.Symbols) != nil || cachedSymbols.Symbols.Hash != symbols.Hash {
						return PrepareResult{}, fmt.Errorf("symbol cache verify failed for %q", request.Root)
					}
				}
			}
			tracePrepareCache(request, cache.Event{Kind: "prepare_verify", ModulePath: request.Root, ActionKey: actionKey, ArtifactHash: image.Hash, Reason: "rebuilt prepare state matches cache"})
		} else if err := request.Cache.StorePrepare(prepareAction, output); err != nil {
			tracePrepareCache(request, cache.Event{Kind: "prepare_write_error", ModulePath: request.Root, ActionKey: actionKey, Reason: err.Error()})
		} else {
			tracePrepareCache(request, cache.Event{Kind: "prepare_store", ModulePath: request.Root, ActionKey: actionKey, ArtifactHash: image.Hash, Reason: "stored execution image and test manifest"})
		}
		if symbols != nil {
			if err := request.Context.Err(); err != nil {
				return PrepareResult{}, err
			}
			if err := request.Cache.StoreSymbols(symbolAction, *symbols); err != nil {
				tracePrepareCache(request, cache.Event{Kind: "symbol_write_error", ModulePath: request.Root, ActionKey: actionKey, Reason: err.Error()})
			}
		}
	}
	if release != nil {
		release()
		release = nil
	}
	return result, nil
}

func loadPreparedSymbols(request Request, built Result, image ir.ExecutionImage, actionKey string) (*ir.ProgramSymbols, error) {
	symbolAction := symbolActionForBuilt(request, image.Hash, built)
	cached, err := request.Cache.LookupSymbols(symbolAction)
	if err == nil && cached.Hit && ir.ValidateProgramSymbols(&image, &cached.Symbols) == nil {
		symbols := cached.Symbols
		return &symbols, nil
	}
	release, err := request.Cache.LockSymbols(request.Context, symbolAction)
	if err != nil {
		return nil, err
	}
	defer release()
	cached, err = request.Cache.LookupSymbols(symbolAction)
	if err == nil && cached.Hit && ir.ValidateProgramSymbols(&image, &cached.Symbols) == nil {
		symbols := cached.Symbols
		return &symbols, nil
	}
	symbols, err := linkProgramSymbols(request.Context, image, nil, built.PackageSymbols, request.Optimization)
	if err != nil {
		return nil, err
	}
	if err := request.Context.Err(); err != nil {
		return nil, err
	}
	if err := request.Cache.StoreSymbols(symbolAction, *symbols); err != nil {
		tracePrepareCache(request, cache.Event{Kind: "symbol_write_error", ModulePath: request.Root, ActionKey: actionKey, Reason: err.Error()})
	}
	return symbols, nil
}

func newPrepareCacheAction(request Request, selectedTarget target.Target, mode string, entries []entrySelection, artifacts []cache.PrepareArtifact) (cache.PrepareAction, string, error) {
	modulePaths := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		modulePaths = append(modulePaths, artifact.ModulePath)
	}
	capabilities := capabilitiesForModules(request.HostCapabilities, modulePaths)
	action := cache.NewPrepareAction(Identity(), ir.ExecutionContract, selectedTarget, mode, request.Root, cacheEntryPoints(entries), artifacts, capabilities)
	action.LimitsHash = request.Limits.cacheHash()
	key, err := action.Key()
	return action, key, err
}

func symbolActionForBuilt(request Request, programHash string, built Result) cache.SymbolAction {
	return cache.NewSymbolAction(Identity(), programHash, built.GraphHash, uint8(request.Optimization))
}

func capabilitiesForModules(metadata map[string][]string, modules []string) []string {
	seen := make(map[string]struct{})
	for _, modulePath := range modules {
		for _, capability := range metadata[modulePath] {
			if capability != "" {
				seen[capability] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for capability := range seen {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

func cacheEntryPoints(entries []entrySelection) []cache.EntryPoint {
	out := make([]cache.EntryPoint, len(entries))
	for i, entry := range entries {
		out[i] = cache.EntryPoint{Name: entry.Name, ModulePath: entry.ModulePath, Function: entry.Function}
	}
	return out
}

func tracePrepareCache(request Request, event cache.Event) {
	if request.TraceCache != nil {
		request.TraceCache(event)
	}
}
