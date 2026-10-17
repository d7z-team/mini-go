package bootstrap_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/d7z-team/mini-go/compiler/bootstrap"
	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
	"github.com/d7z-team/mini-go/compiler/cache"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
	artifact "github.com/d7z-team/mini-go/runtime/bytecode"
)

func buildCompilerImage(t testing.TB) artifact.ExecutionImage {
	t.Helper()
	cacheRoot, err := cache.ResolveDiskRoot("")
	if err != nil {
		t.Fatal(err)
	}
	image, err := bootstrap.BuildCompilerImage(bootstrap.BuildOptions{
		Filesystem: os.DirFS("../.."), Root: ".", Cache: cache.NewDiskBackend(cacheRoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func instantiateCompilerImage(t *testing.T, image artifact.ExecutionImage) *minigoruntime.Instance {
	t.Helper()
	program, err := minigoruntime.LoadExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := program.Instantiate(context.Background(), compilerImageInstanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Error(err)
		}
	})
	return instance
}

func compilerImageInstanceOptions() minigoruntime.InstanceOptions {
	return minigoruntime.InstanceOptions{
		Limits: minigoruntime.Limits{
			MaxSteps:      2_000_000_000,
			MaxGuestBytes: 8 << 30,
		},
	}
}

func callCompilerImage(t *testing.T, instance *minigoruntime.Instance, input []byte) compilerentry.Response {
	t.Helper()
	result, err := instance.CallEntry(context.Background(), minigoruntime.HostBytes(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Values) != 1 {
		t.Fatalf("compiler result = %#v", result.Values)
	}
	output, ok := result.Values[0].Bytes()
	if !ok {
		t.Fatalf("compiler output = %#v", result.Values[0])
	}
	var response compilerentry.Response
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	return response
}
