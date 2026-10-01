package minigo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	minigo "github.com/d7z-team/mini-go"
	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/ffi"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
)

type engineTestBridge struct{ capabilities []string }

func (b engineTestBridge) HostCapabilities() []string {
	return append([]string(nil), b.capabilities...)
}

func (engineTestBridge) Open(context.Context) (ffi.Session, error) { return engineTestSession{}, nil }

type engineTestSession struct{}

func (engineTestSession) Start(context.Context, ffi.Request, ffi.Completion) (ffi.Call, error) {
	return nil, errors.New("unexpected test host call")
}

func (engineTestSession) Shutdown(context.Context) error { return nil }

func newTestEngine(t *testing.T, sources workspace.SourceSet) *minigo.Engine {
	t.Helper()
	cacheRoot, err := cache.ResolveDiskRoot("")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := minigo.New(minigo.Config{Sources: sources, Cache: cache.NewDiskBackend(cacheRoot)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	return engine
}

func runTestEntry(t *testing.T, sources workspace.SourceSet, modulePath, entryName, function string, options minigoruntime.InstanceOptions) minigoruntime.HostValue {
	t.Helper()
	engine := newTestEngine(t, sources)
	program, result, err := engine.Compile(modulePath, minigo.EntryPoint{Name: entryName, Function: function})
	if err != nil || !result.OK() {
		t.Fatalf("compile: result=%#v err=%v", result, err)
	}
	instance, err := program.Instantiate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	run, callErr := instance.Call(context.Background(), entryName)
	closeErr := instance.Close()
	if callErr != nil || closeErr != nil || len(run.Values) != 1 {
		t.Fatalf("call: result=%#v callErr=%v closeErr=%v", run, callErr, closeErr)
	}
	return run.Values[0]
}

func TestEngineCompilesAndRunsSource(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc main() {}\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, sources)
	result, diagnostics, err := engine.Run(context.Background(), "example/main", minigoruntime.InstanceOptions{})
	if err != nil || !diagnostics.OK() {
		t.Fatalf("run: result=%#v diagnostics=%#v err=%v", result, diagnostics, err)
	}
}

func TestEngineForwardsCompilerCapacity(t *testing.T) {
	text := "package main\nfunc Main() int { return 42 }\n"
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: text}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{len(text) - 1, len(text)} {
		engine, err := minigo.New(minigo.Config{
			Sources: sources, Cache: cache.NewMemoryBackend(), Limits: compiler.Limits{MaxSourceBytes: limit},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Close() })
		result, err := engine.Check("example/main")
		if err != nil {
			t.Fatal(err)
		}
		if limit == len(text) {
			if !result.OK() {
				t.Fatalf("source within capacity: %v", result.Diagnostics)
			}
		} else if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "scanner.source.limit" {
			t.Fatalf("source exceeds capacity: %v", result.Diagnostics)
		}
	}
}

func TestEngineStandardLibraryDoesNotRequireHostForPureCode(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/pure",
		Files: []source.File{{Path: "main.mgo", Text: `package pure
import "fmt"
func Run() string { return fmt.Sprintf("%d", 42) }
`}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, sources)
	for _, phase := range []string{"cold", "warm"} {
		program, result, err := engine.Compile("example/pure", minigo.EntryPoint{Name: "run", Function: "Run"})
		if err != nil || !result.OK() {
			t.Fatalf("%s compile: %v, %#v", phase, err, result.Diagnostics)
		}
		instance, err := program.Instantiate(context.Background(), minigoruntime.InstanceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		run, callErr := instance.Call(context.Background(), "run")
		closeErr := instance.Close()
		if callErr != nil || closeErr != nil || len(run.Values) != 1 {
			t.Fatalf("call = %#v, %v, %v", run, callErr, closeErr)
		}
		if got, _ := run.Values[0].StringValue(); got != "42" {
			t.Fatalf("value = %q", got)
		}
	}
}

func TestEngineReturnsSourceDiagnostics(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc broken( {\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, sources)
	result, err := engine.Check("example/main")
	if err != nil || result.OK() || len(result.Diagnostics) == 0 {
		t.Fatalf("check: result=%#v err=%v", result, err)
	}
}

func TestEngineCompileContextCancellation(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc main() {}\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, sources)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := engine.CompileContext(ctx, "example/main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("CompileContext canceled context = %v", err)
	}
}

func TestEngineRejectsNilSourcesAndNilReceiver(t *testing.T) {
	if _, err := minigo.New(minigo.Config{}); err == nil {
		t.Fatal("nil sources were accepted")
	}
	var engine *minigo.Engine
	if _, err := engine.Check("example/main"); err == nil {
		t.Fatal("nil engine was accepted")
	}
}

func TestEngineRunsPackageTests(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/value",
		Files:      []source.File{{Path: "value.mgo", Text: "package value\nfunc Value() int { return 42 }\n"}},
		TestFiles:  []source.File{{Path: "value_test.mgo", Text: "package value\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 42 { t.Fatal(\"wrong value\") } }\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, sources)
	run, result, err := engine.Test(context.Background(), "example/value", nil, minigoruntime.InstanceOptions{
		FFI: engineTestBridge{capabilities: []string{"console"}},
	})
	if err != nil || !result.OK() || len(result.Tests) != 1 || result.Tests[0].Name != "TestValue" || len(run.Values) != 1 {
		t.Fatalf("test: run=%#v result=%#v err=%v", run, result, err)
	}
}

func TestEngineReusesPersistentCompileCache(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/main",
		Files:      []source.File{{Path: "main.mgo", Text: "package main\nfunc main() {}\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	backend := cache.NewMemoryBackend()
	first, err := minigo.New(minigo.Config{Sources: sources, Cache: backend})
	if err != nil {
		t.Fatal(err)
	}
	if _, result, err := first.Compile("example/main"); err != nil || !result.OK() {
		t.Fatalf("cold compile: result=%#v err=%v", result, err)
	}
	second, err := minigo.New(minigo.Config{Sources: sources, Cache: backend})
	if err != nil {
		t.Fatal(err)
	}
	if _, result, err := second.Compile("example/main"); err != nil || !result.OK() || result.Stats.PackageCacheHits == 0 {
		t.Fatalf("warm compile: result=%#v err=%v", result, err)
	}
}

func TestEngineHotPatchAcrossCodeGenerationModes(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/library",
		Files: []source.File{{Path: "library.mgo", Text: `package library
func Value() int {
	condition := true
	if condition { return 42 }
	return 0
}
`}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	compile := func(level compiler.OptimizationLevel, symbols bool) *minigoruntime.Program {
		engine, newErr := minigo.New(minigo.Config{
			Sources: sources, Cache: cache.NewMemoryBackend(), Optimization: level, Symbols: symbols,
		})
		if newErr != nil {
			t.Fatal(newErr)
		}
		program, result, compileErr := engine.Compile("example/library", minigo.EntryPoint{Name: "value", Function: "Value"})
		if compileErr != nil || !result.OK() {
			t.Fatalf("compile O%d: %#v, %v", level, result.Diagnostics, compileErr)
		}
		return program
	}
	base := compile(compiler.OptimizationNone, true)
	optimized := compile(compiler.OptimizationFull, false)
	instance, err := base.Instantiate(context.Background(), minigoruntime.InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	plan, err := instance.PreparePatch(context.Background(), optimized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	result, err := instance.Call(context.Background(), "value")
	if err != nil || len(result.Values) != 1 {
		t.Fatalf("patched result = %#v, %v", result, err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 42 {
		t.Fatalf("patched value = %#v", result.Values[0])
	}
	withSymbols := compile(compiler.OptimizationDefault, true)
	plan, err = instance.PreparePatch(context.Background(), withSymbols)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	result, err = instance.Call(context.Background(), "value")
	if err != nil || len(result.Values) != 1 {
		t.Fatalf("symbolized patched result = %#v, %v", result, err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 42 {
		t.Fatalf("symbolized patched value = %#v", result.Values[0])
	}
}

func TestEngineOptimizationLevelsPreserveObservableEvaluation(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/effects",
		Files: []source.File{{Path: "effects.mgo", Text: `package effects
var order int
func mark(value int) bool { order = order*10 + value; return true }
func Result() int { order = 0; if mark(1) && mark(2) {}; return order }
func Boom() { panic("boom") }
`}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for level := compiler.OptimizationNone; level <= compiler.OptimizationFull; level++ {
		engine, err := minigo.New(minigo.Config{Sources: sources, Cache: cache.NewMemoryBackend(), Optimization: level})
		if err != nil {
			t.Fatal(err)
		}
		program, result, err := engine.Compile("example/effects",
			minigo.EntryPoint{Name: "result", Function: "Result"},
			minigo.EntryPoint{Name: "boom", Function: "Boom"},
		)
		if err != nil || !result.OK() {
			t.Fatalf("compile O%d: %#v, %v", level, result.Diagnostics, err)
		}
		instance, err := program.Instantiate(context.Background(), minigoruntime.InstanceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		valueResult, err := instance.Call(context.Background(), "result")
		if err != nil || len(valueResult.Values) != 1 {
			t.Fatalf("O%d result = %#v, %v", level, valueResult, err)
		}
		if value, ok := valueResult.Values[0].Int64(); !ok || value != 12 {
			t.Fatalf("O%d evaluation order = %#v", level, valueResult.Values[0])
		}
		if _, err := instance.Call(context.Background(), "boom"); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("O%d panic = %v", level, err)
		}
		if err := instance.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
