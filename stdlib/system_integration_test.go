package stdlib_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/rpc"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/stdlib/host/console"
	oshost "github.com/d7z-team/mini-go/stdlib/host/os"
)

func TestSystemCapabilitiesExecuteInRuntime(t *testing.T) {
	module, err := workspace.LoadSources(t.Context(), "testdata/system", "system", nil)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := workspace.MergeSourceSets(module.Sources, testStandardLibrary(t))
	if err != nil {
		t.Fatal(err)
	}
	cacheRoot, err := cache.ResolveDiskRoot("")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := compiler.Prepare(compiler.Request{
		Root: module.ModulePath, Sources: sources, Cache: cache.New(cache.NewDiskBackend(cacheRoot)),
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
	filesystemBackend, err := oshost.NewMemoryFilesystem(map[string][]byte{"input.txt": []byte("input")})
	if err != nil {
		t.Fatal(err)
	}
	output := console.NewBuffer("")
	bridge := newFFIBridge(t,
		func() (rpc.Provider, error) { return console.NewFmtConsoleProvider(output) },
		func() (rpc.Provider, error) { return oshost.NewFilesystemProvider(filesystemBackend) },
		func() (rpc.Provider, error) {
			return oshost.NewEnvironmentProvider(oshost.Map{"MINIGO_TEST": "rpc"})
		},
	)
	clock := minigoruntime.NewManualClock(time.Unix(123, 456))
	instance, err := program.Instantiate(context.Background(), minigoruntime.InstanceOptions{
		FFI: bridge, Clock: clock, Entropy: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 16)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, runErr := callEntryWithClock(t.Context(), instance, clock)
	closeErr := instance.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if got := output.Stdout(); got != "input|rpc|8\n" {
		t.Fatalf("stdout = %q", got)
	}
	file, err := filesystemBackend.Open("output.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	written := make([]byte, 6)
	if _, err := file.Read(written); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := string(written); got != "input!" {
		t.Fatalf("output.txt = %q", got)
	}
}
