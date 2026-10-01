package stdlib_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/ffi"
	"github.com/d7z-team/mini-go/rpc"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/stdlib"
	"github.com/d7z-team/mini-go/stdlib/host/console"
	oshost "github.com/d7z-team/mini-go/stdlib/host/os"
)

func testStandardLibrary(t testing.TB) workspace.SourceSet {
	t.Helper()
	library := stdlib.Open()
	sources, err := workspace.StandardLibrary(library)
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func newStdlibRuntimeOptions(t *testing.T) minigoruntime.InstanceOptions {
	t.Helper()
	filesystemBackend, err := oshost.NewMemoryFilesystem(nil)
	if err != nil {
		t.Fatal(err)
	}
	return minigoruntime.InstanceOptions{Entropy: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 1024)), FFI: newFFIBridge(
		t,
		func() (rpc.Provider, error) { return console.NewFmtConsoleProvider(console.NewBuffer("")) },
		func() (rpc.Provider, error) { return oshost.NewFilesystemProvider(filesystemBackend) },
		func() (rpc.Provider, error) { return oshost.NewEnvironmentProvider(oshost.Map{}) },
	)}
}

func newFFIBridge(t *testing.T, providers ...func() (rpc.Provider, error)) ffi.Bridge {
	t.Helper()
	values := make([]rpc.Provider, 0, len(providers))
	for _, create := range providers {
		provider, err := create()
		if err != nil {
			t.Fatal(err)
		}
		values = append(values, provider)
	}
	bridge, err := rpc.NewHost(rpc.HostOptions{Providers: values})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	return bridge
}

func callEntryWithClock(ctx context.Context, instance *minigoruntime.Instance, clock *minigoruntime.ManualClock) (minigoruntime.RunResult, error) {
	execution, err := instance.StartEntry()
	if err != nil {
		return minigoruntime.RunResult{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			execution.Cancel()
			return minigoruntime.RunResult{}, err
		}
		state, pollErr := execution.Poll()
		if pollErr != nil && !errors.Is(pollErr, minigoruntime.ErrBusy) {
			return minigoruntime.RunResult{}, pollErr
		}
		switch state {
		case minigoruntime.ExecutionCompleted, minigoruntime.ExecutionFailed, minigoruntime.ExecutionCanceled:
			return execution.Result()
		case minigoruntime.ExecutionPaused:
			return minigoruntime.RunResult{}, errors.New("stdlib test execution paused")
		}
		if pollErr == nil && state == minigoruntime.ExecutionPending && clock.AdvanceToNext() {
			continue
		}
		select {
		case <-ctx.Done():
			execution.Cancel()
			return minigoruntime.RunResult{}, ctx.Err()
		default:
			time.Sleep(100 * time.Microsecond)
		}
	}
}

func prepareStdlibProgram(t testing.TB, root, sourceText string, entries []compiler.EntryPoint) *minigoruntime.Program {
	t.Helper()
	script, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: root,
		Files:      []source.File{{Path: "main.mgo", Text: sourceText}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := workspace.MergeSourceSets(script, testStandardLibrary(t))
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot, err := cache.ResolveDiskRoot("")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := compiler.Prepare(compiler.Request{
		Root: root, Sources: sources, Cache: cache.New(cache.NewDiskBackend(cacheRoot)), EntryPoints: entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Checked.OK() || prepared.Image == nil {
		t.Fatalf("compile diagnostics = %#v", prepared.Checked.Diagnostics)
	}
	program, err := minigoruntime.LoadExecutionImage(*prepared.Image)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func prepareStdlibRunProgram(t testing.TB, root, sourceText string) *minigoruntime.Program {
	t.Helper()
	return prepareStdlibProgram(t, root, sourceText, []compiler.EntryPoint{{Name: "run", ModulePath: root, Function: "Run"}})
}

func runFuzzString(program *minigoruntime.Program, arguments ...minigoruntime.HostValue) (string, string) {
	instance, err := program.Instantiate(context.Background(), minigoruntime.InstanceOptions{Limits: minigoruntime.Limits{
		MaxSteps: 500_000, MaxCallDepth: 256, MaxAllocatedBytes: 64 << 20,
		MaxStringBytes: 1 << 20, MaxCollectionElements: 10_000,
	}})
	if err != nil {
		return "", err.Error()
	}
	result, runErr := instance.Call(context.Background(), "run", arguments...)
	closeErr := instance.Close()
	if runErr != nil {
		return "", runErr.Error()
	}
	if closeErr != nil {
		return "", closeErr.Error()
	}
	if len(result.Values) != 1 {
		return "", fmt.Sprintf("returned %d values", len(result.Values))
	}
	value, ok := result.Values[0].StringValue()
	if !ok {
		return "", "result is not a string"
	}
	return value, ""
}

func requirePassingTestReport(t testing.TB, report minigoruntime.RunResult) {
	t.Helper()
	if len(report.Values) != 1 {
		t.Fatalf("test entry returned %d values, want testing.Report", len(report.Values))
	}
	fields, ok := report.Values[0].Fields()
	if !ok {
		t.Fatalf("test entry result = %#v, want testing.Report", report.Values[0])
	}
	passed, valid := false, false
	var failures []string
	for _, field := range fields {
		switch field.Name {
		case "Passed":
			passed, valid = field.Value.Bool()
		case "Results":
			results, _ := field.Value.Items()
			for _, item := range results {
				resultFields, _ := item.Fields()
				name, status, message := "", "", ""
				for _, resultField := range resultFields {
					value, _ := resultField.Value.StringValue()
					switch resultField.Name {
					case "Name":
						name = value
					case "Status":
						status = value
					case "Message":
						message = value
					}
				}
				if status == "fail" {
					failures = append(failures, name+": "+message)
				}
			}
		}
	}
	if !valid || !passed {
		t.Fatalf("MiniGo tests failed: %v", failures)
	}
}
