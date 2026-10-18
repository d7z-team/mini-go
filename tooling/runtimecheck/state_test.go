package runtimecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestStateVectorsPreserveFailedStartAccountingAndDeterminism(t *testing.T) {
	input := []byte(fmt.Sprintf(`{"module":{"path":"test","package":"main"},"constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],"functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"code":{"types":[{"kind":3,"primitive":3}],"instructions":[[%d,0,0,1],[%d,0,1,1]],"blocks":[{"first":0,"count":1,"fallthrough":1,"cost":1},{"first":1,"count":1,"fallthrough":4294967295,"cost":1}],"descriptors":{"const":[{"constant":"answer"}],"return":[{"result_count":1}]},"operands":[{"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]}}]}`, bytecode.OpConst, bytecode.OpReturn))
	inputs := map[string][]byte{"entry": input}
	encoded, err := GenerateStateVectors(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := GenerateStateVectors(context.Background(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, repeated) {
		t.Fatal("same owner actions produced different observations")
	}
	var vectors []StateVector
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	var failed, exact *StateVector
	for index := range vectors {
		switch vectors[index].Limit {
		case 127:
			failed = &vectors[index]
		case 128:
			exact = &vectors[index]
		}
	}
	if failed == nil || exact == nil {
		t.Fatalf("missing exact allocation boundary vectors: failed=%v exact=%v", failed != nil, exact != nil)
	}
	if len(failed.Actions) != 4 {
		t.Fatalf("failed boundary actions: %+v", failed.Actions)
	}
	for _, action := range failed.Actions {
		if action.Operation != "start" || action.Error != "execution.allocation_limit" || action.Memory != failed.InitializationMemory {
			t.Fatalf("failed frame charge reused or leaked memory: %+v", action)
		}
	}
	if len(exact.Actions) == 0 || exact.Actions[0].Operation != "start" || exact.Actions[0].Error != "" {
		t.Fatalf("exact boundary did not admit the frame: %+v", exact.Actions)
	}
	for _, action := range exact.Actions[1:] {
		if action.Error != "" {
			t.Fatalf("exact boundary execution failed after admission: %+v", exact.Actions)
		}
	}
}

func TestStateVectorsRejectMalformedInputBeforePublication(t *testing.T) {
	for _, input := range []string{
		`{"functions":`,
		`[]`,
		`[{"module":{"path":"test","package":"main"}},{"module":{"path":"test","package":"main"}}]`,
	} {
		encoded, err := GenerateStateVectors(context.Background(), map[string][]byte{"broken": []byte(input)})
		if err == nil || !strings.Contains(err.Error(), "broken") || encoded != nil {
			t.Fatalf("input %q, data %q, error %v", input, encoded, err)
		}
	}
}

func TestCallBudgetFailureIsRetryableAndHighBudgetCompletes(t *testing.T) {
	for _, name := range []string{"memory_call.json", "memory_tail_call.json"} {
		t.Run(name, func(t *testing.T) {
			input, err := os.ReadFile("../../testdata/runtime/" + name)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := GenerateStateVectors(context.Background(), map[string][]byte{name: input})
			if err != nil {
				t.Fatal(err)
			}
			var vectors []StateVector
			if err := json.Unmarshal(encoded, &vectors); err != nil {
				t.Fatal(err)
			}
			var low, high *StateVector
			for index := range vectors {
				if vectors[index].Limit == 160 {
					low = &vectors[index]
				}
				if vectors[index].Limit == 4096 {
					high = &vectors[index]
				}
			}
			if low == nil || high == nil {
				t.Fatalf("missing budget vectors: low=%v high=%v", low != nil, high != nil)
			}
			if len(high.Actions) == 0 {
				t.Fatal("high budget produced no actions")
			}
			completed := false
			for _, action := range high.Actions {
				if action.Error != "" {
					t.Fatalf("high budget action failed: %+v", action)
				}
				if action.State == runtime.ExecutionCompleted {
					completed = true
				}
			}
			if !completed {
				t.Fatalf("high budget did not complete: %+v", high.Actions)
			}
			for _, action := range low.Actions {
				if action.Error != "" && action.Error != "execution.allocation_limit" {
					t.Fatalf("low budget poisoned the execution after admission failure: %+v", action)
				}
				if action.Memory[1] != 0 {
					t.Fatalf("low budget leaked a reservation: %+v", action)
				}
			}
		})
	}
}
