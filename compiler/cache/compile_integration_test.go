package cache_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
	"github.com/d7z-team/mini-go/stdlib"
)

type testWorkspaceOptions struct {
	Cache        cache.Backend
	CacheVerify  bool
	CacheHash    bool
	TraceCache   func(cache.Event)
	Target       target.Target
	Optimization compiler.OptimizationLevel
}

func compileWorkspace(packages []workspace.SourcePackage, options testWorkspaceOptions) (compiler.Result, error) {
	sources, err := workspace.NewMemorySourceSet(packages)
	if err != nil {
		return compiler.Result{}, err
	}
	return compileSourceSet(packages[0].ModulePath, sources, options)
}

func compileSourceSet(root string, sources workspace.SourceSet, options testWorkspaceOptions) (compiler.Result, error) {
	var compileCache cache.Cache
	if options.Cache != nil {
		compileCache = cache.New(options.Cache)
	}
	return compiler.Compile(compiler.Request{
		Root: root, Target: options.Target, Sources: sources, Cache: compileCache,
		CacheVerify: options.CacheVerify, CacheHash: options.CacheHash, TraceCache: options.TraceCache,
		Optimization: options.Optimization,
	})
}

func TestCompileCacheSeparatesOptimizationLevels(t *testing.T) {
	packages := []workspace.SourcePackage{{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nfunc Value() int { return 1 }\n"}}}}
	backend := cache.NewMemoryBackend()
	none, err := compileWorkspace(packages, testWorkspaceOptions{Cache: backend, Optimization: compiler.OptimizationNone})
	if err != nil || !none.OK() {
		t.Fatalf("O0 compile: %#v, %v", none.Diagnostics, err)
	}
	optimized, err := compileWorkspace(packages, testWorkspaceOptions{Cache: backend, Optimization: compiler.OptimizationDefault})
	if err != nil || !optimized.OK() || optimized.Stats.PackageCacheHits != 0 {
		t.Fatalf("cold O1 compile: stats=%#v diagnostics=%#v err=%v", optimized.Stats, optimized.Diagnostics, err)
	}
	warm, err := compileWorkspace(packages, testWorkspaceOptions{Cache: backend, Optimization: compiler.OptimizationDefault})
	if err != nil || !warm.OK() || warm.Stats.PackageCacheHits == 0 {
		t.Fatalf("warm O1 compile: stats=%#v diagnostics=%#v err=%v", warm.Stats, warm.Diagnostics, err)
	}
	if none.ExportHashes["example/main"] != optimized.ExportHashes["example/main"] {
		t.Fatalf("optimization changed export hash: %s != %s", none.ExportHashes["example/main"], optimized.ExportHashes["example/main"])
	}
}

func TestCompileCacheIsolatesBuildTagsAndHashesExcludedCandidates(t *testing.T) {
	packages := func(excludedBody string) []workspace.SourcePackage {
		return []workspace.SourcePackage{{ModulePath: "example/main", Files: []source.File{
			{Path: "main.mgo", Text: "package main\nfunc Main() int { return mode() }\n"},
			{Path: "debug.mgo", Text: "//go:build debug\n\npackage main\nfunc mode() int { return 1 }\n"},
			{Path: "release.mgo", Text: "//go:build !debug\n\npackage main\nfunc mode() int { " + excludedBody + " }\n"},
		}}}
	}
	backend := cache.NewMemoryBackend()
	debug := target.Target{Tags: []string{"debug"}}
	first, err := compileWorkspace(packages("return 2"), testWorkspaceOptions{Cache: backend, Target: debug})
	if err != nil || !first.OK() {
		t.Fatalf("initial build failed: %v %#v", err, first.Diagnostics)
	}
	var events []cache.Event
	second, err := compileWorkspace(packages("return 3"), testWorkspaceOptions{
		Cache: backend, Target: debug, TraceCache: func(event cache.Event) { events = append(events, event) },
	})
	if err != nil || !second.OK() {
		t.Fatalf("excluded edit build failed: %v %#v", err, second.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/main") {
		t.Fatalf("excluded candidate edit did not invalidate cache: %#v", events)
	}
	events = nil
	third, err := compileWorkspace(packages("return 3"), testWorkspaceOptions{
		Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) },
	})
	if err != nil || !third.OK() {
		t.Fatalf("default target build failed: %v %#v", err, third.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/main") || first.Target.Equal(third.Target) {
		t.Fatalf("target did not isolate cache: events=%#v first=%#v third=%#v", events, first.Target, third.Target)
	}
}

func TestCompileCacheHitsRepeatedBuild(t *testing.T) {
	backend := cache.NewMemoryBackend()
	packages := cachedWorkspacePackages("return lib.Value()\n", "func Value() int { return 41 }\n")
	first, err := compileWorkspace(packages, testWorkspaceOptions{Cache: backend})
	if err != nil || !first.OK() {
		t.Fatalf("first build failed: %v %#v", err, first.Diagnostics)
	}
	firstHash := artifactHash(t, first.Artifacts["example/main"])
	var events []cache.Event
	second, err := compileWorkspace(packages, testWorkspaceOptions{Cache: backend, CacheHash: true, TraceCache: func(event cache.Event) { events = append(events, event) }})
	if err != nil || !second.OK() {
		t.Fatalf("second build failed: %v %#v", err, second.Diagnostics)
	}
	if !hasCacheEvent(events, "hit", "example/lib") || !hasCacheEvent(events, "hit", "example/main") {
		t.Fatalf("expected package cache hits, got %#v", events)
	}
	if second.Stats.PackagesScanned != 2 || second.Stats.PackagesParsed != 0 || second.Stats.PackagesAnalyzed != 0 || second.Stats.PackagesLowered != 0 || second.Stats.PackagesCompiled != 0 || second.Stats.PackageCacheHits != 2 {
		t.Fatalf("warm build did unnecessary front-end work: %#v", second.Stats)
	}
	var action cache.Action
	for _, event := range events {
		if event.Kind == "hash" && event.ModulePath == "example/main" {
			if err := json.Unmarshal(event.MaterialJSON, &action); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if action.Compiler != compiler.Identity() || action.IntrinsicSchema != ir.IntrinsicSchema {
		t.Fatalf("incomplete cache identity: %#v", action)
	}
	if got := artifactHash(t, second.Artifacts["example/main"]); got != firstHash {
		t.Fatalf("cached build changed artifact hash: %s != %s", got, firstHash)
	}
}

func TestCompileCacheReusesPackagesWithAdditiveStandardSources(t *testing.T) {
	application, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files: []source.File{{Path: "main.mgo", Text: `package main
import "strings"
func Main() string { return strings.ToUpper("value") }
`}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	backend := cache.NewMemoryBackend()
	compileSources := func(additional bool, trace func(cache.Event)) compiler.Result {
		t.Helper()
		standardSources, err := workspace.StandardLibrary(stdlib.Open())
		if err != nil {
			t.Fatal(err)
		}
		if additional {
			extra, err := workspace.StandardLibrary(fstest.MapFS{
				"unused/unused.mgo": {Data: []byte("package unused\nconst Value = 1\n")},
			})
			if err != nil {
				t.Fatal(err)
			}
			standardSources, err = workspace.ComposeStandardSourceSets(standardSources, extra)
			if err != nil {
				t.Fatal(err)
			}
		}
		sources, err := workspace.MergeSourceSets(application, standardSources)
		if err != nil {
			t.Fatal(err)
		}
		result, err := compileSourceSet("example/main", sources, testWorkspaceOptions{Cache: backend, TraceCache: trace})
		if err != nil || !result.OK() {
			t.Fatalf("compile standard sources: err=%v diagnostics=%#v", err, result.Diagnostics)
		}
		return result
	}
	compileSources(false, nil)
	var events []cache.Event
	result := compileSources(true, func(event cache.Event) { events = append(events, event) })
	if !hasCacheEvent(events, "hit", "strings") || !hasCacheEvent(events, "hit", "example/main") {
		t.Fatalf("unchanged packages missed cache across standard source compositions: %#v", events)
	}
	if result.Stats.PackagesParsed != 0 || result.Stats.PackagesAnalyzed != 0 || result.Stats.PackagesCompiled != 0 {
		t.Fatalf("warm standard source build repeated compiler work: %#v", result.Stats)
	}
}

func TestCompileCacheRestoresCanonicalComplexConstant(t *testing.T) {
	packages := func(mainDeclarations string) []workspace.SourcePackage {
		return []workspace.SourcePackage{
			{
				ModulePath: "example/main",
				Files: []source.File{{Path: "main.mgo", Text: `package main
import "example/lib"
func Main() complex128 { return lib.Value }
` + mainDeclarations}},
			},
			{
				ModulePath: "example/lib",
				Files: []source.File{{Path: "lib.mgo", Text: `package lib
const Third = 1.0 / 3.0
const Value = complex(Third, -Third)
`}},
			},
		}
	}
	backend := cache.NewMemoryBackend()
	first, err := compileWorkspace(packages(""), testWorkspaceOptions{Cache: backend})
	if err != nil || !first.OK() {
		t.Fatalf("initial build failed: %v %#v", err, first.Diagnostics)
	}
	var events []cache.Event
	second, err := compileWorkspace(packages("func local() int { return 1 }\n"), testWorkspaceOptions{
		Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) },
	})
	if err != nil || !second.OK() {
		t.Fatalf("cached dependency build failed: %v %#v", err, second.Diagnostics)
	}
	if !hasCacheEvent(events, "hit", "example/lib") || !hasCacheEvent(events, "miss", "example/main") {
		t.Fatalf("expected cached dependency and rebuilt importer, got %#v", events)
	}
}

func TestCompileCacheRebindsImplementationOnlyDependencyChange(t *testing.T) {
	backend := cache.NewMemoryBackend()
	first, err := compileWorkspace(cachedWorkspacePackages("return lib.Value()\n", "func Value() int { return 41 }\n"), testWorkspaceOptions{Cache: backend})
	if err != nil || !first.OK() {
		t.Fatalf("first build failed: %v %#v", err, first.Diagnostics)
	}
	firstDependencyHash := artifactHash(t, first.Artifacts["example/lib"])
	var events []cache.Event
	second, err := compileWorkspace(cachedWorkspacePackages("return lib.Value()\n", "func Value() int { return 42 }\n"), testWorkspaceOptions{
		Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) },
	})
	if err != nil || !second.OK() {
		t.Fatalf("second build failed: %v %#v", err, second.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/lib") || !hasCacheEvent(events, "hit", "example/main") {
		t.Fatalf("expected dependency miss and importer hit, got %#v", events)
	}
	if second.Stats.PackagesParsed != 1 || second.Stats.PackagesAnalyzed != 1 || second.Stats.PackagesLowered != 1 || second.Stats.PackagesCompiled != 1 || second.Stats.PackageCacheHits != 1 || second.Stats.PackageCacheMisses != 1 {
		t.Fatalf("implementation-only edit rebuilt the wrong packages: %#v", second.Stats)
	}
	dependencyHash := artifactHash(t, second.Artifacts["example/lib"])
	if dependencyHash == firstDependencyHash {
		t.Fatal("dependency implementation edit did not change artifact")
	}
	main := second.Artifacts["example/main"]
	if len(main.Requirements) != 1 || main.Requirements[0].Hash != dependencyHash {
		t.Fatalf("cached importer was not rebound: %#v", main.Requirements)
	}
}

func TestCompileCacheInvalidatesDependencyExportChange(t *testing.T) {
	backend := cache.NewMemoryBackend()
	first := cachedWorkspacePackages("return lib.Value()\n", "func Value() int { return 41 }\n")
	if result, err := compileWorkspace(first, testWorkspaceOptions{Cache: backend}); err != nil || !result.OK() {
		t.Fatalf("first build failed: %v %#v", err, result.Diagnostics)
	}
	second := cachedWorkspacePackages("return lib.Value()\n", "func Value() int { return 41 }\nfunc Added() int { return 1 }\n")
	var events []cache.Event
	result, err := compileWorkspace(second, testWorkspaceOptions{Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) }})
	if err != nil || !result.OK() {
		t.Fatalf("second build failed: %v %#v", err, result.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/lib") || !hasCacheEvent(events, "miss", "example/main") {
		t.Fatalf("expected export change to invalidate importer, got %#v", events)
	}
}

func TestCompileCacheStopsInvalidationWhenIntermediateExportsStayStable(t *testing.T) {
	packages := func(base string) []workspace.SourcePackage {
		return []workspace.SourcePackage{
			{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/middle\"\nfunc Main() int { return middle.Value() }\n"}}},
			{ModulePath: "example/middle", Files: []source.File{{Path: "middle.mgo", Text: "package middle\nimport \"example/base\"\nfunc Value() int { return base.Value() }\n"}}},
			{ModulePath: "example/base", Files: []source.File{{Path: "base.mgo", Text: "package base\n" + base}}},
		}
	}
	backend := cache.NewMemoryBackend()
	if result, err := compileWorkspace(packages("func Value() int { return 1 }\n"), testWorkspaceOptions{Cache: backend}); err != nil || !result.OK() {
		t.Fatalf("initial build failed: %v %#v", err, result.Diagnostics)
	}
	var events []cache.Event
	result, err := compileWorkspace(packages("func Value() int { return 1 }\nfunc Added() int { return 2 }\n"), testWorkspaceOptions{Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) }})
	if err != nil || !result.OK() {
		t.Fatalf("incremental build failed: %v %#v", err, result.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/base") || !hasCacheEvent(events, "miss", "example/middle") || !hasCacheEvent(events, "hit", "example/main") {
		t.Fatalf("export invalidation did not stop at stable intermediate exports: %#v", events)
	}
	if result.Stats.PackagesCompiled != 2 || result.Stats.PackageCacheHits != 1 || result.Stats.PackageCacheMisses != 2 {
		t.Fatalf("unexpected transitive invalidation stats: %#v", result.Stats)
	}
}

func TestCompileCacheInvalidatesGenericTemplateChange(t *testing.T) {
	backend := cache.NewMemoryBackend()
	main := "return lib.Transform(21)\n"
	first := cachedWorkspacePackages(main, "func Transform[T ~int](value T) T { return value }\n")
	if result, err := compileWorkspace(first, testWorkspaceOptions{Cache: backend}); err != nil || !result.OK() {
		t.Fatalf("first build failed: %v %#v", err, result.Diagnostics)
	}
	second := cachedWorkspacePackages(main, "func Transform[T ~int](value T) T { return value + value }\n")
	var events []cache.Event
	result, err := compileWorkspace(second, testWorkspaceOptions{Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) }})
	if err != nil || !result.OK() {
		t.Fatalf("second build failed: %v %#v", err, result.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/main") {
		t.Fatalf("generic template change did not invalidate importer: %#v", events)
	}
}

func TestCompileCacheVerifyDetectsWrongHit(t *testing.T) {
	backend := cache.NewMemoryBackend()
	pkg := singlePackage("return 1\n")
	var actionMaterial []byte
	result, err := compileWorkspace([]workspace.SourcePackage{pkg}, testWorkspaceOptions{
		Cache: backend, CacheHash: true,
		TraceCache: func(event cache.Event) {
			if event.Kind == "hash" {
				actionMaterial = append([]byte(nil), event.MaterialJSON...)
			}
		},
	})
	if err != nil || !result.OK() {
		t.Fatalf("initial build failed: %v %#v", err, result.Diagnostics)
	}
	var action cache.Action
	if err := json.Unmarshal(actionMaterial, &action); err != nil {
		t.Fatal(err)
	}
	wrong, err := compileWorkspace([]workspace.SourcePackage{singlePackage("return 2\n")}, testWorkspaceOptions{})
	if err != nil || !wrong.OK() {
		t.Fatalf("wrong build failed: %v %#v", err, wrong.Diagnostics)
	}
	wrongArtifact := wrong.Artifacts["example/main"]
	wrongSymbols := wrong.PackageSymbols["example/main"]
	wrongData, err := cache.FromArtifact(wrongArtifact)
	if err != nil {
		t.Fatal(err)
	}
	store := cache.New(backend)
	sealed, err := cache.SealArtifact(wrongArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreCompile(action, sealed, wrongSymbols, wrongData); err != nil {
		t.Fatal(err)
	}
	_, err = compileWorkspace([]workspace.SourcePackage{pkg}, testWorkspaceOptions{Cache: backend, CacheVerify: true})
	if err == nil || !strings.Contains(err.Error(), "compile cache verify failed") {
		t.Fatalf("expected verify mismatch, got %v", err)
	}
}

func TestCompileContinuesWhenCacheBackendFails(t *testing.T) {
	backend := unavailableBackend{}
	var events []cache.Event
	result, err := compileWorkspace([]workspace.SourcePackage{singlePackage("return 1\n")}, testWorkspaceOptions{
		Cache: backend, TraceCache: func(event cache.Event) { events = append(events, event) },
	})
	if err != nil || !result.OK() {
		t.Fatalf("compile failed: %v %#v", err, result.Diagnostics)
	}
	if !hasCacheEvent(events, "miss", "example/main") || !hasCacheEvent(events, "write_error", "example/main") {
		t.Fatalf("cache failures were not traced: %#v", events)
	}
}

type unavailableBackend struct{}

func (unavailableBackend) GetAction(cache.ActionID) (cache.Entry, bool, error) {
	return cache.Entry{}, false, errors.New("cache unavailable")
}

func (unavailableBackend) GetOutput(cache.OutputID) ([]byte, bool, error) {
	return nil, false, errors.New("cache unavailable")
}

func (unavailableBackend) PutOutput(cache.OutputID, []byte) error {
	return errors.New("cache unavailable")
}

func (unavailableBackend) PutAction(cache.ActionID, cache.Entry) error {
	return errors.New("cache unavailable")
}

func cachedWorkspacePackages(mainBody, libDeclarations string) []workspace.SourcePackage {
	return []workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/lib\"\nfunc Main() int {\n" + mainBody + "}\n"}},
	}, {
		ModulePath: "example/lib",
		Files:      []source.File{{Path: "lib.mgo", Text: "package lib\n" + libDeclarations}},
	}}
}

func singlePackage(body string) workspace.SourcePackage {
	return workspace.SourcePackage{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nfunc Main() int {\n" + body + "}\n"}}}
}

func hasCacheEvent(events []cache.Event, kind, modulePath string) bool {
	for _, event := range events {
		if event.Kind == kind && event.ModulePath == modulePath {
			return true
		}
	}
	return false
}

func artifactHash(t *testing.T, artifact ir.Artifact) string {
	t.Helper()
	hash, err := ir.Hash(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
