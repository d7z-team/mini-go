package compiler_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestCompilerCachePreservesLimitDiagnostics(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "sample", Files: []source.File{{Path: "main.mgo", Text: `package main
func Keep[T any](v T) T { return v }
func main() { _ = Keep[int](1); _ = Keep[string]("value") }
`}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, storage := range []string{"memory", "transient", "disk"} {
		for _, operation := range []string{"compile", "prepare"} {
			for _, limits := range []compiler.Limits{{MaxASTNodes: 2}, {MaxTokens: 8}, {MaxSyntaxDepth: 2}, {MaxSpecializations: 1}} {
				t.Run(fmt.Sprintf("%s/%s/%+v", storage, operation, limits), func(t *testing.T) {
					var shared cache.Cache
					switch storage {
					case "memory":
						shared = cache.New(cache.NewMemoryBackend())
					case "transient":
						shared = cache.NewTransient(cache.TransientConfig{})
					case "disk":
						shared = cache.New(cache.NewDiskBackend(t.TempDir()))
					}
					run := func(store cache.Cache, limits compiler.Limits) []source.Diagnostic {
						request := compiler.Request{Root: "sample", Sources: sources, Cache: store, Limits: limits}
						if operation == "compile" {
							result, err := compiler.Compile(request)
							if err != nil {
								t.Fatal(err)
							}
							return result.Diagnostics
						}
						result, err := compiler.Prepare(request)
						if err != nil {
							t.Fatal(err)
						}
						return result.Checked.Diagnostics
					}
					if diagnostics := run(shared, compiler.Limits{}); source.HasErrors(diagnostics) {
						t.Fatal(diagnostics)
					}
					cold := run(nil, limits)
					if !source.HasErrors(cold) {
						t.Fatalf("limit did not reject input: %+v", limits)
					}
					if warm := run(shared, limits); !reflect.DeepEqual(warm, cold) {
						t.Fatalf("warm=%v; cold=%v", warm, cold)
					}
					if diagnostics := run(shared, compiler.Limits{}); source.HasErrors(diagnostics) {
						t.Fatal(diagnostics)
					}
				})
			}
		}
	}
}

func TestPrepareCacheReusesLinkedImageAcrossSessions(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc main() {}\nfunc Answer() int { return 42 }\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	backend := cache.NewMemoryBackend()
	request := compiler.Request{Root: "example/main", Sources: sources, Cache: cache.New(backend)}
	first, err := compiler.Prepare(request)
	if err != nil || first.Image == nil || !first.Checked.OK() {
		t.Fatalf("first Prepare = %#v, %v", first, err)
	}
	request.Cache = cache.New(backend)
	warm, err := compiler.Prepare(request)
	if err != nil || warm.Image == nil || warm.Image.Hash != first.Image.Hash {
		t.Fatalf("warm Prepare = %#v, %v", warm, err)
	}
	stats := warm.Checked.Stats
	if stats.PackagesParsed != 0 || stats.PackagesAnalyzed != 0 || stats.PackagesLowered != 0 || stats.PackagesCompiled != 0 || stats.PackageCacheMisses != 0 || stats.PrepareCacheHits != 1 || stats.PrepareCacheMisses != 0 || stats.ImagesLinked != 0 {
		t.Fatalf("warm Prepare performed work: %#v", stats)
	}

	request.EntryPoints = []compiler.EntryPoint{{Name: "answer", ModulePath: "example/main", Function: "Answer"}}
	changed, err := compiler.Prepare(request)
	if err != nil || changed.Image == nil || !changed.Checked.OK() {
		t.Fatalf("changed entry Prepare = %#v, %v", changed, err)
	}
	if changed.Checked.Stats.PrepareCacheMisses != 1 || changed.Checked.Stats.ImagesLinked != 1 {
		t.Fatalf("changed entry reused prepare output: %#v", changed.Checked.Stats)
	}
}

func TestImportCycleCannotBeHiddenByCompilerCache(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example/a", Files: []source.File{{Path: "a.mgo", Text: "package a\nimport \"example/b\"\n"}}},
		{ModulePath: "example/b", Files: []source.File{{Path: "b.mgo", Text: "package b\nimport \"example/a\"\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	shared := cache.New(cache.NewMemoryBackend())
	request := compiler.Request{Root: "example/a", Sources: sources, Cache: shared}
	checked, err := compiler.Check(request)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(request)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		prepared, prepareErr := compiler.Prepare(request)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		assertDiagnosticCode(t, prepared.Checked.Diagnostics, "compiler.workspace.module.cycle")
		if prepared.Image != nil || prepared.Checked.Stats.PrepareCacheHits != 0 {
			t.Fatalf("cycle Prepare(%d) = %#v", attempt, prepared)
		}
	}
	assertDiagnosticCode(t, checked.Diagnostics, "compiler.workspace.module.cycle")
	assertDiagnosticCode(t, compiled.Diagnostics, "compiler.workspace.module.cycle")
}

func assertDiagnosticCode(t *testing.T, diagnostics []source.Diagnostic, code source.DiagnosticCode) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("missing diagnostic %q in %#v", code, diagnostics)
}

func TestPrepareCacheIncludesLoweringDiscoveredDependencies(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "fmt", Files: []source.File{{Path: "builtin.mgo", Text: "package fmt\nfunc print(args ...any) {}\nfunc println(args ...any) {}\n"}}},
		{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nfunc main() { println(42) }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &packageStateRejectingBackend{Backend: cache.NewMemoryBackend()}
	request := compiler.Request{Root: "example/main", Sources: sources, Cache: cache.New(backend)}
	first, err := compiler.Prepare(request)
	if err != nil || first.Image == nil || !first.Checked.OK() {
		t.Fatalf("first Prepare = %#v, %v", first, err)
	}
	if got := first.Checked.Order; len(got) != 2 || got[0] != "fmt" || got[1] != "example/main" {
		t.Fatalf("compile order = %#v", got)
	}
	request.Cache = cache.New(backend)
	backend.reject = true
	warm, err := compiler.Prepare(request)
	if err != nil || warm.Image == nil || warm.Image.Hash != first.Image.Hash {
		t.Fatalf("warm Prepare = %#v, %v", warm, err)
	}
	if backend.fullReads != 0 || warm.Checked.GraphHash != first.Checked.GraphHash || warm.Checked.Stats.PrepareCacheHits != 1 || warm.Checked.Stats.PackagesParsed != 0 || warm.Checked.Stats.ImagesLinked != 0 {
		t.Fatalf("warm Prepare did not reuse the dynamic closure: %#v", warm.Checked)
	}
}

type packageStateRejectingBackend struct {
	cache.Backend
	reject    bool
	fullReads int
}

func (b *packageStateRejectingBackend) GetOutput(id cache.OutputID) ([]byte, bool, error) {
	data, found, err := b.Backend.GetOutput(id)
	if err == nil && found && b.reject && bytes.Contains(data, []byte(`"format":"mini-go-package-state"`)) {
		b.fullReads++
		return nil, false, errors.New("full package state read")
	}
	return data, found, err
}

func TestPrepareCacheHitDoesNotReadFullPackageState(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example/lib", Files: []source.File{{Path: "lib.mgo", Text: "package lib\nfunc Value() int { return 42 }\n"}}},
		{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/lib\"\nfunc main() { _ = lib.Value() }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &packageStateRejectingBackend{Backend: cache.NewMemoryBackend()}
	first, err := compiler.Prepare(compiler.Request{Root: "example/main", Sources: sources, Cache: cache.New(backend)})
	if err != nil || first.Image == nil || !first.Checked.OK() {
		t.Fatalf("first Prepare = %#v, %v", first, err)
	}
	backend.reject = true
	warm, err := compiler.Prepare(compiler.Request{Root: "example/main", Sources: sources, Cache: cache.New(backend)})
	if err != nil || warm.Image == nil || warm.Image.Hash != first.Image.Hash {
		t.Fatalf("warm Prepare = %#v, %v", warm, err)
	}
	if backend.fullReads != 0 || warm.Checked.Stats.PrepareCacheHits != 1 || warm.Checked.Stats.PackagesParsed != 0 || warm.Checked.Stats.ImagesLinked != 0 {
		t.Fatalf("warm Prepare read package state or relinked: %#v", warm.Checked.Stats)
	}
}

func TestPrepareCacheTracksDependencyImplementation(t *testing.T) {
	backend := cache.NewMemoryBackend()
	build := func(value int) compiler.PrepareResult {
		sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
			{ModulePath: "example/lib", Files: []source.File{{Path: "lib.mgo", Text: fmt.Sprintf("package lib\nfunc Value() int { return %d }\n", value)}}},
			{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/lib\"\nfunc main() { _ = lib.Value() }\n"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := compiler.Prepare(compiler.Request{Root: "example/main", Sources: sources, Cache: cache.New(backend)})
		if err != nil || prepared.Image == nil || !prepared.Checked.OK() {
			t.Fatalf("Prepare(%d) = %#v, %v", value, prepared, err)
		}
		return prepared
	}
	first := build(1)
	changed := build(2)
	if changed.Image.Hash == first.Image.Hash {
		t.Fatal("dependency implementation change reused execution image")
	}
	stats := changed.Checked.Stats
	if stats.PackageCacheHits != 1 || stats.PackageCacheMisses != 1 || stats.PrepareCacheMisses != 1 || stats.ImagesLinked != 1 {
		t.Fatalf("dependency implementation change rebuilt wrong closure: %#v", stats)
	}
}

func TestPrepareCacheTracksEmbeddedResourceContent(t *testing.T) {
	backend := cache.NewMemoryBackend()
	build := func(value string) compiler.PrepareResult {
		sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
			ModulePath: "example/main",
			Files:      []source.File{{Path: "main.mgo", Text: "package main\nimport _ \"embed\"\n//go:embed value.txt\nvar value string\nfunc main() { _ = value }\n"}},
			Resources:  []workspace.ResourceFile{{Path: "value.txt", Data: []byte(value)}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := compiler.Prepare(compiler.Request{Root: "example/main", Sources: sources, Cache: cache.New(backend)})
		if err != nil || prepared.Image == nil || !prepared.Checked.OK() {
			t.Fatalf("Prepare(%q) = %#v, %v", value, prepared, err)
		}
		return prepared
	}
	first := build("first")
	second := build("second")
	if second.Image.Hash == first.Image.Hash || second.Checked.Stats.PackageCacheMisses != 1 || second.Checked.Stats.PrepareCacheMisses != 1 {
		t.Fatalf("resource change reused cached output: first=%s second=%s stats=%#v", first.Image.Hash, second.Image.Hash, second.Checked.Stats)
	}
}

func TestPrepareCacheVerifyDetectsDifferentImage(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc main() {}\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	shared := cache.New(cache.NewMemoryBackend())
	first, err := compiler.Prepare(compiler.Request{Root: "example/main", Sources: sources, Cache: shared})
	if err != nil || first.Image == nil {
		t.Fatalf("first Prepare = %#v, %v", first, err)
	}
	wrong := *first.Image
	wrong.Hash = "different"
	_, err = compiler.Prepare(compiler.Request{
		Root: "example/main", Sources: sources, Cache: prepareOverrideCache{
			Cache: shared, output: cache.PreparedOutput{Image: wrong},
		}, CacheVerify: true,
	})
	if err == nil || !strings.Contains(err.Error(), "prepare cache verify failed") {
		t.Fatalf("Prepare verification error = %v", err)
	}
}

type prepareOverrideCache struct {
	cache.Cache
	output cache.PreparedOutput
}

func (c prepareOverrideCache) LookupPrepare(cache.PrepareAction) (cache.PrepareLookup, error) {
	return cache.PrepareLookup{Image: c.output.Image, TestManifest: c.output.TestManifest, Hit: true, Reason: "test override"}, nil
}
