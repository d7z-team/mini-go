package integrations

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/ffi"
	"github.com/d7z-team/mini-go/rpc"
	"github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/runtime/bytecode"
	"github.com/d7z-team/mini-go/stdlib"
	service "github.com/d7z-team/mini-go/testdata/rpc/generated/go/service"
	types "github.com/d7z-team/mini-go/testdata/rpc/generated/go/types"
	"github.com/d7z-team/mini-go/tooling/rpccheck"
)

func TestRPCPrecompiledImages(t *testing.T) {
	for _, name := range []string{"call", "provider"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			file, err := os.Open("../testdata/rpc/images/" + name + ".json.gz")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			compressed, err := gzip.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			defer compressed.Close()
			data, err := io.ReadAll(compressed)
			if err != nil {
				t.Fatal(err)
			}
			var image bytecode.ExecutionImage
			if err := json.Unmarshal(data, &image); err != nil {
				t.Fatal(err)
			}
			program, err := runtime.LoadExecutionImage(image)
			if err != nil {
				t.Fatal(err)
			}
			handler := &rpccheck.Laboratory{}
			provider, err := service.NewLaboratoryProvider(handler)
			if err != nil {
				t.Fatal(err)
			}
			published := make(chan rpc.Provider, 1)
			host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}, PublishProvider: func(_ context.Context, provider rpc.Provider) (func() error, error) {
				published <- provider
				return func() error { return nil }, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer host.Shutdown(context.Background())
			instance, err := program.Instantiate(ctx, runtime.InstanceOptions{FFI: host})
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			if name == "call" {
				result, err := instance.Call(ctx, "default")
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Values) != 1 {
					t.Fatalf("results: %v", result.Values)
				}
				value, ok := result.Values[0].Int64()
				if !ok || value != 42 {
					t.Fatalf("result: %v", result.Values)
				}
				if handler.Released.Load() != 1 {
					t.Fatalf("resource release count: %d", handler.Released.Load())
				}
			} else {
				execution, err := instance.Start("default")
				if err != nil {
					t.Fatal(err)
				}
				finished := make(chan error, 1)
				go func() { _, err := execution.Wait(ctx); finished <- err }()
				defer func() { instance.Close(); <-finished }()
				var guest rpc.Provider
				select {
				case guest = <-published:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				binder, err := rpc.NewLocalBinder(rpc.LocalBinderOptions{}, guest)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := rpccheck.Exercise(ctx, binder); err != nil {
					result, executionErr := execution.Result()
					t.Logf("provider state=%v result=%+v error=%v", execution.State(), result, executionErr)
					t.Fatal(err)
				}
				if err := instance.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRPCPendingCallAndResourceSurviveInstancePatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const root = "rpcfixture/hot_patch"
	backend := cache.NewMemoryBackend()
	oldProgram := prepareRPCPatchProgram(t, root, rpcPatchMainSource("0"), backend)
	newProgram := prepareRPCPatchProgram(t, root, rpcPatchMainSource("1"), backend)
	handler := &hotPatchLaboratory{started: make(chan struct{}), release: make(chan struct{})}
	provider, err := service.NewLaboratoryProvider(handler)
	if err != nil {
		t.Fatal(err)
	}
	host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	bridge := &countingRPCBridge{host: host}
	instance, err := oldProgram.Instantiate(ctx, runtime.InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	handler.releaseOnCleanup(t)

	execution, err := instance.Start("WaitAndOpen")
	if err != nil {
		t.Fatal(err)
	}
	started := false
	for !started {
		state, pollErr := execution.Poll()
		if pollErr != nil {
			t.Fatal(pollErr)
		}
		select {
		case <-handler.started:
			started = true
		case <-ctx.Done():
			t.Fatal("RPC handler did not receive the pending call")
		default:
			time.Sleep(time.Millisecond)
		}
		if state == runtime.ExecutionFailed || state == runtime.ExecutionCanceled {
			t.Fatalf("RPC execution ended before handler started: %s", state)
		}
	}
	if execution.State() != runtime.ExecutionPending {
		t.Fatalf("RPC execution state = %s, want pending", execution.State())
	}
	if got := bridge.opens.Load(); got != 1 {
		t.Fatalf("FFI sessions before patch = %d, want 1", got)
	}
	plan, err := instance.PreparePatch(ctx, newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if got := bridge.opens.Load(); got != 1 {
		t.Fatalf("patch reopened FFI session: %d", got)
	}
	handler.releaseWait()
	result, err := execution.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 73 {
		t.Fatalf("pending RPC result after patch = %#v, want 73", result.Values)
	}
	retained, err := instance.Call(ctx, "ReadRetained")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := retained.Values[0].Int64(); !ok || value != 41 {
		t.Fatalf("global resource after patch = %#v, want 41", retained.Values)
	}
}

func TestRPCRetainedResourceCloseSurvivesInstancePatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const root = "rpcfixture/hot_patch_close"
	backend := cache.NewMemoryBackend()
	oldProgram := prepareRPCPatchProgram(t, root, rpcPatchMainSource("0"), backend)
	newProgram := prepareRPCPatchProgram(t, root, rpcPatchMainSource("1"), backend)
	handler := &hotPatchLaboratory{
		started: make(chan struct{}), release: make(chan struct{}),
		closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
	}
	provider, err := service.NewLaboratoryProvider(handler)
	if err != nil {
		t.Fatal(err)
	}
	host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	bridge := &countingRPCBridge{host: host}
	instance, err := oldProgram.Instantiate(ctx, runtime.InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	handler.releaseOnCleanup(t)
	t.Cleanup(handler.releaseClose)

	openExecution, err := instance.Start("WaitAndOpen")
	if err != nil {
		t.Fatal(err)
	}
	for {
		state, pollErr := openExecution.Poll()
		if pollErr != nil {
			t.Fatal(pollErr)
		}
		select {
		case <-handler.started:
			goto opened
		case <-ctx.Done():
			t.Fatal("RPC handler did not receive the pending call")
		default:
		}
		if state == runtime.ExecutionFailed || state == runtime.ExecutionCanceled {
			t.Fatalf("RPC execution ended before handler started: %s", state)
		}
		time.Sleep(time.Millisecond)
	}
opened:
	handler.releaseWait()
	if _, err := openExecution.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	closeExecution, err := instance.Start("CloseRetained")
	if err != nil {
		t.Fatal(err)
	}
	for {
		state, pollErr := closeExecution.Poll()
		if pollErr != nil {
			t.Fatal(pollErr)
		}
		select {
		case <-handler.closeStarted:
			goto closing
		case <-ctx.Done():
			t.Fatal("resource Close did not start")
		default:
		}
		if state == runtime.ExecutionFailed || state == runtime.ExecutionCanceled {
			t.Fatalf("Close execution ended before resource Close started: %s", state)
		}
		time.Sleep(time.Millisecond)
	}
closing:
	plan, err := instance.PreparePatch(ctx, newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if got := bridge.opens.Load(); got != 1 {
		t.Fatalf("patch reopened FFI session while Close was pending: %d", got)
	}
	handler.releaseClose()
	result, err := closeExecution.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 99 {
		t.Fatalf("Close result after patch = %#v, want 99", result.Values)
	}
	if got := handler.closeCount.Load(); got != 1 {
		t.Fatalf("resource Close count = %d, want 1", got)
	}
}

func TestRPCFailedPatchKeepsPendingCallAndSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const root = "rpcfixture/hot_patch_failed"
	backend := cache.NewMemoryBackend()
	oldProgram := prepareRPCPatchProgram(t, root, rpcPatchMainSource("0"), backend)
	invalidProgram := prepareRPCPatchProgram(t, root, rpcPatchInvalidMainSource(), backend)
	handler := &hotPatchLaboratory{started: make(chan struct{}), release: make(chan struct{})}
	provider, err := service.NewLaboratoryProvider(handler)
	if err != nil {
		t.Fatal(err)
	}
	host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Shutdown(context.Background())
	bridge := &countingRPCBridge{host: host}
	instance, err := oldProgram.Instantiate(ctx, runtime.InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	handler.releaseOnCleanup(t)

	execution, err := instance.Start("WaitAndOpen")
	if err != nil {
		t.Fatal(err)
	}
	for {
		state, pollErr := execution.Poll()
		if pollErr != nil {
			t.Fatal(pollErr)
		}
		select {
		case <-handler.started:
			goto pending
		case <-ctx.Done():
			t.Fatal("RPC handler did not receive the pending call")
		default:
		}
		if state == runtime.ExecutionFailed || state == runtime.ExecutionCanceled {
			t.Fatalf("RPC execution ended before handler started: %s", state)
		}
		time.Sleep(time.Millisecond)
	}
pending:
	if _, err := instance.PreparePatch(ctx, invalidProgram); err == nil {
		t.Fatal("incompatible patch was accepted")
	}
	if got := bridge.opens.Load(); got != 1 {
		t.Fatalf("failed patch changed FFI session count: %d", got)
	}
	handler.releaseWait()
	result, err := execution.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 73 {
		t.Fatalf("pending RPC result after failed patch = %#v, want 73", result.Values)
	}
	retained, err := instance.Call(ctx, "ReadRetained")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := retained.Values[0].Int64(); !ok || value != 40 {
		t.Fatalf("resource after failed patch = %#v, want 40", retained.Values)
	}
}

func prepareRPCPatchProgram(t *testing.T, root, main string, backend cache.Backend) *runtime.Program {
	t.Helper()
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	application, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: root, Files: []source.File{{Path: "main.mgo", Text: main}}},
		{ModulePath: "rpcfixture/generated/mgo/service", Files: []source.File{{Path: "binding.mgo", Text: read("../testdata/rpc/generated/mgo/service/binding.mgo")}}},
		{ModulePath: "rpcfixture/generated/mgo/types", Files: []source.File{{Path: "binding.mgo", Text: read("../testdata/rpc/generated/mgo/types/binding.mgo")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	standard, err := workspace.StandardLibrary(stdlib.Open())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := workspace.MergeSourceSets(application, standard)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := compiler.Prepare(compiler.Request{
		Root: root, Sources: sources, Cache: cache.New(backend), EntryPoints: []compiler.EntryPoint{
			{Name: "WaitAndOpen", ModulePath: root, Function: "WaitAndOpen"},
			{Name: "ReadRetained", ModulePath: root, Function: "ReadRetained"},
			{Name: "CloseRetained", ModulePath: root, Function: "CloseRetained"},
		},
	})
	if err != nil || !prepared.Checked.OK() || prepared.Image == nil {
		t.Fatalf("prepare RPC patch program: %v %+v", err, prepared.Checked.Diagnostics)
	}
	program, err := runtime.LoadExecutionImage(*prepared.Image)
	if err != nil {
		t.Fatal(err)
	}
	return program
}

func rpcPatchMainSource(offset string) string {
	return `package main

import (
    "rpc"
    service "rpcfixture/generated/mgo/service"
    types "rpcfixture/generated/mgo/types"
)

var bound *service.LaboratoryClient
var retained types.Counter

func WaitAndOpen() int64 {
    client, err := service.BindLaboratoryClient(rpc.ClientOptions{})
    if err != nil { panic(err) }
    bound = client
    counter, _, err := client.Open(40)
    if err != nil { panic(err) }
    retained = counter
    value, err := client.Wait()
    if err != nil { panic(err) }
    return value
}

func ReadRetained() int64 {
    value, err := bound.Read(retained)
    if err != nil { panic(err) }
    return value + ` + offset + `
}

func CloseRetained() int64 {
    err := retained.Close()
    if err != nil { panic(err) }
    return 99 + ` + offset + `
}
`
}

func rpcPatchInvalidMainSource() string {
	return rpcPatchMainSource("0") + `
var incompatible int64
`
}

type countingRPCBridge struct {
	host  *rpc.Host
	opens atomic.Int64
}

func (b *countingRPCBridge) Open(ctx context.Context) (ffi.Session, error) {
	b.opens.Add(1)
	return b.host.Open(ctx)
}

type hotPatchLaboratory struct {
	started          chan struct{}
	release          chan struct{}
	startOnce        sync.Once
	releaseOnce      sync.Once
	closeStarted     chan struct{}
	closeRelease     chan struct{}
	closeReleaseOnce sync.Once
	closeCount       atomic.Int32
}

func (h *hotPatchLaboratory) releaseOnCleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(h.releaseWait)
}

func (h *hotPatchLaboratory) releaseWait() {
	h.releaseOnce.Do(func() { close(h.release) })
}

func (h *hotPatchLaboratory) releaseClose() {
	if h.closeRelease != nil {
		h.closeReleaseOnce.Do(func() { close(h.closeRelease) })
	}
}

func (h *hotPatchLaboratory) Echo(_ context.Context, packet service.Packet) (service.Packet, error) {
	return packet, nil
}

func (h *hotPatchLaboratory) Tree(_ context.Context, node service.Node) (service.Node, error) {
	return node, nil
}

func (h *hotPatchLaboratory) Open(_ context.Context, initial int64) (types.CounterHandler, types.Details, error) {
	counter := &hotPatchCounter{closeStarted: h.closeStarted, closeRelease: h.closeRelease, closeCount: &h.closeCount}
	counter.value.Store(initial)
	return counter, types.Details{Label: "counter"}, nil
}

func (h *hotPatchLaboratory) Read(ctx context.Context, counter types.CounterHandler) (int64, error) {
	return counter.Add(ctx, 0)
}

func (h *hotPatchLaboratory) Wait(ctx context.Context) (int64, error) {
	h.startOnce.Do(func() { close(h.started) })
	select {
	case <-h.release:
		return 73, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type hotPatchCounter struct {
	value        atomic.Int64
	closed       atomic.Bool
	closeStarted chan struct{}
	closeRelease chan struct{}
	closeOnce    sync.Once
	closeCount   *atomic.Int32
}

func (c *hotPatchCounter) Add(_ context.Context, delta int64) (int64, error) {
	if c.closed.Load() {
		return 0, rpc.StatusError{Code: rpc.CodeNotFound, Message: "counter is closed"}
	}
	return c.value.Add(delta), nil
}

func (c *hotPatchCounter) Close(ctx context.Context) error {
	if c.closeCount != nil {
		c.closeCount.Add(1)
	}
	c.closed.Store(true)
	if c.closeStarted != nil {
		c.closeOnce.Do(func() { close(c.closeStarted) })
		select {
		case <-c.closeRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
