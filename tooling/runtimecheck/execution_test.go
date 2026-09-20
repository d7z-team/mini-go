package runtimecheck

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestExecutionOracleProducesSealedProgramsAndObservedResults(t *testing.T) {
	data, err := GenerateExecutionVectors(os.DirFS("../../testdata/runtime/source"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []ExecutionVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("../../testdata/runtime/execution_expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		if vector.ResultInteger != want[vector.Name] {
			t.Fatalf("%s O%d returned %s", vector.Name, vector.Optimization, vector.ResultInteger)
		}
		program, err := runtime.LoadExecutionImage(*vector.Image)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := program.WithSymbols(*vector.Symbols); err != nil {
			t.Fatal(err)
		}
		if vector.Image.CompilerID != bytecode.CompilerIdentity {
			t.Fatal("oracle identity is stale")
		}
	}
}

func TestDistributedExecutionImagesMatchIndependentResults(t *testing.T) {
	file, err := os.Open("../../testdata/runtime/execution.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	var vectors []ExecutionVector
	if err := json.NewDecoder(compressed).Decode(&vectors); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../testdata/runtime/execution_expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]string
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		t.Run(vector.Name+"/O"+strconv.Itoa(int(vector.Optimization)), func(t *testing.T) {
			want, ok := expected[vector.Name]
			if !ok {
				t.Fatal("missing independent result")
			}
			program, err := runtime.LoadExecutionImage(*vector.Image)
			if err != nil {
				t.Fatal(err)
			}
			parallelism := []int{1}
			if strings.HasPrefix(vector.Name, "parallel_") || vector.Name == "select_transaction" {
				parallelism = []int{1, 2, 4}
			}
			for _, workers := range parallelism {
				instance, err := program.Instantiate(context.Background(), runtime.InstanceOptions{Parallelism: workers})
				if err != nil {
					t.Fatal(err)
				}
				result, callErr := instance.Call(context.Background(), "default")
				closeErr := instance.Close()
				if callErr != nil {
					t.Fatal(callErr)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				value, ok := result.Values[0].Int64()
				if !ok || strconv.FormatInt(value, 10) != want {
					t.Fatalf("parallelism %d: result %v, want %s", workers, result.Values, want)
				}
			}
		})
	}
}
