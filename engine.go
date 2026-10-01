// Package minigo provides the high-level API for embedding the Mini-Go compiler
// and runtime in a Go program.
package minigo

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/stdlib"
)

// UnlimitedSteps disables the cumulative runtime instruction budget.
const UnlimitedSteps = minigoruntime.UnlimitedSteps

type Config struct {
	Sources        workspace.SourceSet
	Libraries      *LibrarySet
	Target         target.Target
	Cache          cache.Backend
	Optimization   compiler.OptimizationLevel
	Limits         compiler.Limits
	Symbols        bool
	TransientCache cache.TransientConfig
}

type EntryPoint struct {
	Name     string
	Function string
}

type Result struct {
	Diagnostics []source.Diagnostic
	Stats       compiler.Stats
	Tests       []Test
}

type Test struct {
	Package string
	Name    string
	Index   int
}

func (r Result) OK() bool { return !source.HasErrors(r.Diagnostics) }

type Engine struct {
	compiler     *compiler.Compiler
	capabilities []HostCapability
}

func New(config Config) (*Engine, error) {
	if config.Sources == nil {
		return nil, errors.New("mini-go requires application sources")
	}
	coreSources, err := workspace.StandardLibrary(stdlib.Open())
	if err != nil {
		return nil, err
	}
	standardSources := []workspace.SourceSet{coreSources}
	if config.Libraries != nil {
		for _, library := range config.Libraries.standards {
			standardSources = append(standardSources, library.sources)
		}
	}
	standardLibrary, err := workspace.ComposeStandardSourceSets(standardSources...)
	if err != nil {
		return nil, err
	}
	sourceSets := []workspace.SourceSet{config.Sources, standardLibrary}
	if config.Libraries != nil {
		for _, module := range config.Libraries.modules {
			sourceSets = append(sourceSets, module.sources)
		}
	}
	sources, err := workspace.MergeSourceSets(sourceSets...)
	if err != nil {
		return nil, err
	}
	var compiledCache cache.Cache
	if config.Cache != nil {
		compiledCache = cache.New(config.Cache)
	}
	frontend, err := compiler.New(compiler.Options{
		Sources: sources, Target: config.Target, Cache: compiledCache,
		Optimization:     config.Optimization,
		Limits:           config.Limits,
		Symbols:          config.Symbols,
		TransientCache:   config.TransientCache,
		HostCapabilities: hostCapabilitiesByPackage(config.Libraries),
	})
	if err != nil {
		return nil, err
	}
	engine := &Engine{compiler: frontend, capabilities: stdlib.HostCapabilities()}
	if config.Libraries != nil {
		engine.capabilities = append(engine.capabilities, config.Libraries.capabilities...)
		slices.Sort(engine.capabilities)
		engine.capabilities = slices.Compact(engine.capabilities)
	}
	return engine, nil
}

// Close releases compiler session state. Sources and persistent cache
// backends remain owned by the embedding application.
func (e *Engine) Close() error {
	if e == nil || e.compiler == nil {
		return nil
	}
	err := e.compiler.Close()
	e.compiler = nil
	e.capabilities = nil
	return err
}

func hostCapabilitiesByPackage(libraries *LibrarySet) map[string][]string {
	out := make(map[string][]string)
	if libraries != nil {
		for path, capabilities := range libraries.packageCapabilities {
			names := make([]string, len(capabilities))
			for i, capability := range capabilities {
				names[i] = string(capability)
			}
			out[path] = names
		}
	}
	return out
}

// AvailableHostCapabilities returns the host services declared by the embedded
// standard library and registered standard source libraries.
func (e *Engine) AvailableHostCapabilities() []HostCapability {
	if e == nil {
		return nil
	}
	return append([]HostCapability(nil), e.capabilities...)
}

func (e *Engine) Check(root string) (Result, error) {
	return e.CheckContext(context.Background(), root)
}

// CheckContext checks root and cooperatively stops when ctx is canceled.
func (e *Engine) CheckContext(ctx context.Context, root string) (Result, error) {
	if e == nil || e.compiler == nil {
		return Result{}, errors.New("nil mini-go engine")
	}
	checked, err := e.compiler.CheckContext(ctx, root)
	return Result{Diagnostics: checked.Diagnostics, Stats: checked.Stats}, err
}

func (e *Engine) Compile(root string, entries ...EntryPoint) (*minigoruntime.Program, Result, error) {
	return e.CompileContext(context.Background(), root, entries...)
}

// CompileContext compiles root and cooperatively stops when ctx is canceled.
func (e *Engine) CompileContext(ctx context.Context, root string, entries ...EntryPoint) (*minigoruntime.Program, Result, error) {
	if e == nil || e.compiler == nil {
		return nil, Result{}, errors.New("nil mini-go engine")
	}
	compilerEntries := make([]compiler.EntryPoint, len(entries))
	for i, entry := range entries {
		compilerEntries[i] = compiler.EntryPoint{Name: entry.Name, ModulePath: root, Function: entry.Function}
	}
	prepared, err := e.compiler.PrepareContext(ctx, root, compilerEntries)
	result := Result{Diagnostics: prepared.Checked.Diagnostics, Stats: prepared.Checked.Stats}
	if err != nil || prepared.Image == nil {
		return nil, result, err
	}
	program, err := minigoruntime.LoadExecutionImage(*prepared.Image)
	if err == nil && prepared.Symbols != nil {
		program, err = program.WithSymbols(*prepared.Symbols)
	}
	return program, result, err
}

func (e *Engine) Run(ctx context.Context, root string, options minigoruntime.InstanceOptions) (run minigoruntime.RunResult, result Result, err error) {
	program, result, err := e.CompileContext(ctx, root)
	if err != nil || program == nil {
		return minigoruntime.RunResult{}, result, err
	}
	instance, err := program.Instantiate(ctx, options)
	if err != nil {
		return minigoruntime.RunResult{}, result, err
	}
	defer func() { err = errors.Join(err, instance.Close()) }()
	run, err = instance.CallMain(ctx)
	return run, result, err
}

func (e *Engine) Test(ctx context.Context, root string, indexes []int, options minigoruntime.InstanceOptions) (run minigoruntime.RunResult, result Result, err error) {
	if e == nil || e.compiler == nil {
		return minigoruntime.RunResult{}, Result{}, errors.New("nil mini-go engine")
	}
	preparedByRoot, err := e.compiler.PrepareTestsContext(ctx, []string{root})
	if err != nil {
		return minigoruntime.RunResult{}, Result{}, err
	}
	prepared := preparedByRoot[root]
	tests := make([]Test, len(prepared.TestManifest))
	for i, test := range prepared.TestManifest {
		tests[i] = Test{Package: test.Package, Name: test.Name, Index: test.Index}
	}
	result = Result{Diagnostics: prepared.Checked.Diagnostics, Stats: prepared.Checked.Stats, Tests: tests}
	if prepared.Image == nil {
		return minigoruntime.RunResult{}, result, nil
	}
	program, err := minigoruntime.LoadExecutionImage(*prepared.Image)
	if err != nil {
		return minigoruntime.RunResult{}, result, err
	}
	if prepared.Symbols != nil {
		program, err = program.WithSymbols(*prepared.Symbols)
		if err != nil {
			return minigoruntime.RunResult{}, result, err
		}
	}
	instance, err := program.Instantiate(ctx, options)
	if err != nil {
		return minigoruntime.RunResult{}, result, err
	}
	defer func() { err = errors.Join(err, instance.Close()) }()
	if indexes == nil {
		run, err = instance.CallEntry(ctx)
		return run, result, err
	}
	seen := make(map[int]struct{}, len(indexes))
	values := make([]minigoruntime.HostValue, len(indexes))
	for i, index := range indexes {
		if index < 0 || index >= len(prepared.TestManifest) {
			return minigoruntime.RunResult{}, result, fmt.Errorf("test selection index %d is outside manifest", index)
		}
		if _, exists := seen[index]; exists {
			return minigoruntime.RunResult{}, result, fmt.Errorf("test selection index %d is duplicated", index)
		}
		seen[index] = struct{}{}
		values[i] = minigoruntime.HostInt("Int", int64(index))
	}
	run, err = instance.Call(ctx, compiler.TestSelectionEntry, minigoruntime.HostSlice("Slice<Int>", values...))
	return run, result, err
}
