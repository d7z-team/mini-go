package runtime

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// These shared programs cover storage transfers that hand-built heap graphs
// cannot exercise: rebind, return/unwind, pointer aliases and container writes.
// Collection runs through the live instance's stopped-owner boundary, and
// poisoning makes a stale nursery view observable on its next read.
func TestManagedExecutionSurvivesCollectionAtEveryInstruction(t *testing.T) {
	want := map[string]int64{
		"channel_buffer": 42, "defer_result": 42, "frame_cell_rebind": 42,
		"closure": 8, "recover_panic": 42,
		"loop_capture": 333, "managed_graph_churn": 42, "map_range": 35,
		"nested_value_storage": 42,
		"select_transaction":   42, "select_wait": 42,
		"stdlib_reflect_fields": 42,
		"stdlib_reflect_slice":  42,
	}
	seen := make(map[string]bool, len(want))
	file, err := os.Open("../testdata/runtime/execution.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	decoder := json.NewDecoder(compressed)
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	for decoder.More() {
		var vector struct {
			Name         string          `json:"name"`
			Optimization int             `json:"optimization"`
			Image        json.RawMessage `json:"image"`
		}
		if err := decoder.Decode(&vector); err != nil {
			t.Fatal(err)
		}
		expected, selected := want[vector.Name]
		if !selected {
			continue
		}
		seen[vector.Name] = true
		t.Run(fmt.Sprintf("%s/O%d", vector.Name, vector.Optimization), func(t *testing.T) {
			var image ir.ExecutionImage
			if err := json.Unmarshal(vector.Image, &image); err != nil {
				t.Fatal(err)
			}
			program, err := LoadExecutionImage(image)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := program.Instantiate(t.Context(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = instance.Close() })
			instance.supervisor.Load().stop()
			execution, err := instance.Start("default")
			if err != nil {
				t.Fatal(err)
			}
			instance.vm.heap.poisonCollected = true
			for iteration := 0; ; iteration++ {
				instance.vm.heap.collectionRequested.Store(true)
				state, _, err := execution.PollSteps(1)
				if err != nil {
					t.Fatalf("instruction boundary %d: %v", iteration, err)
				}
				if state == ExecutionCompleted {
					break
				}
				if iteration%7 == 0 {
					if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
						t.Fatal(err)
					}
					err := instance.vm.collectMajorManaged()
					instance.vm.leaveOwner()
					if err != nil {
						t.Fatalf("major at boundary %d: %v", iteration, err)
					}
				}
			}
			result, err := execution.Result()
			if err != nil || len(result.Values) != 1 {
				t.Fatalf("result = %v, %v", result.Values, err)
			}
			if actual, ok := result.Values[0].Int64(); !ok || actual != expected {
				t.Fatalf("result = %v, want %d", result.Values, expected)
			}
			stats := instance.vm.heap.stats()
			if stats.minorCollections == 0 || stats.majorCollections == 0 || stats.copiedBytes == 0 {
				t.Fatalf("execution did not exercise relocation and both generations: %+v", stats)
			}
		})
	}
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("shared execution fixture %q is missing", name)
		}
	}
}
