package integrations_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/workspace"
	miniruntime "github.com/d7z-team/mini-go/runtime"
)

func TestTypeDispatchPreservesOrderedCasesBindingsAndEvaluation(t *testing.T) {
	sources, err := workspace.DiscoverIndexedSourceTree(os.DirFS("testdata/type_dispatch"), ".", "sample")
	if err != nil {
		t.Fatal(err)
	}
	backend := cache.NewMemoryBackend()
	for level := compiler.OptimizationNone; level <= compiler.OptimizationFull; level++ {
		t.Run(fmt.Sprintf("O%d", level), func(t *testing.T) {
			compiled, err := compiler.Prepare(compiler.Request{Root: "sample", Sources: sources, Cache: cache.New(backend), EntryPoints: []compiler.EntryPoint{{Name: "main", Function: "Main"}}, Optimization: level})
			if err != nil || compiled.Image == nil {
				t.Fatalf("prepare: %v %v", err, compiled.Checked.Diagnostics)
			}
			program, err := miniruntime.LoadExecutionImage(*compiled.Image)
			if err != nil {
				t.Fatal(err)
			}
			for _, quantum := range []int{1, 2, 64} {
				instance, err := program.Instantiate(t.Context(), miniruntime.InstanceOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer instance.Close()
				execution, err := instance.Start("main")
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 10000; i++ {
					state, steps, err := execution.PollSteps(quantum)
					if err != nil || steps > quantum {
						t.Fatalf("poll: %v/%d %v", state, steps, err)
					}
					if state != miniruntime.ExecutionRunning && state != miniruntime.ExecutionPending {
						break
					}
					if i == 9999 {
						t.Fatal("type dispatch did not complete")
					}
				}
				result, err := execution.Wait(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				requireIntegrationInt(t, result.Values[0], 42)
				instance.Close()
			}
		})
	}
}
