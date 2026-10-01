package stdlib_test

import (
	"context"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
)

func TestPackageTestsExecuteInRuntime(t *testing.T) {
	sources := testStandardLibrary(t)
	paths, err := sources.PackagePaths()
	if err != nil {
		t.Fatal(err)
	}
	packages := make([]string, 0, len(paths))
	for _, modulePath := range paths {
		pkg, ok, err := sources.Package(modulePath)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("stdlib package %s disappeared", modulePath)
		}
		if len(pkg.TestFiles) != 0 {
			packages = append(packages, modulePath)
		}
	}
	cacheRoot, err := cache.ResolveDiskRoot("")
	if err != nil {
		t.Fatal(err)
	}
	session, err := compiler.New(compiler.Options{
		Sources: sources,
		Cache:   cache.New(cache.NewDiskBackend(cacheRoot)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close compiler: %v", err)
		}
	})
	prepared, err := session.PrepareTests(packages)
	if err != nil {
		t.Fatal(err)
	}
	for _, modulePath := range packages {
		t.Run(modulePath, func(t *testing.T) {
			result := prepared[modulePath]
			if !result.Checked.OK() || result.Image == nil {
				t.Fatalf("compile diagnostics: %#v", result.Checked.Diagnostics)
			}
			program, err := minigoruntime.LoadExecutionImage(*result.Image)
			if err != nil {
				t.Fatal(err)
			}
			clock := minigoruntime.NewManualClock(time.Unix(1_700_000_000, 123_456_789))
			options := newStdlibRuntimeOptions(t)
			options.Clock = clock
			instance, err := program.Instantiate(context.Background(), options)
			if err != nil {
				t.Fatalf("instantiate test program: %v", err)
			}
			report, runErr := callEntryWithClock(t.Context(), instance, clock)
			closeErr := instance.Close()
			if runErr != nil {
				t.Fatal(runErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			requirePassingTestReport(t, report)
		})
	}
}
