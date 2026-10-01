package runtimecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestStateVectorsPreserveFailedStartAccountingAndDeterminism(t *testing.T) {
	input := []byte(fmt.Sprintf(`{"module":{"path":"test","package":"main"},"constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],"functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"code":{"types":[{"kind":3,"primitive":3}],"instructions":[[%d,0,0],[%d,0,1]],"descriptors":{"const":[{"constant":"answer"}],"return":[{"result_count":1}]},"operands":[{"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]}}]}`, bytecode.OpConst, bytecode.OpReturn))
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
	for _, vector := range vectors {
		if vector.Limit != 128 {
			continue
		}
		if len(vector.Actions) != 4 {
			t.Fatalf("constrained invocation actions: %+v", vector.Actions)
		}
		for _, action := range vector.Actions {
			if action.Operation != "start" || action.Error != "execution.allocation_limit" || action.Memory != [4]int64{} {
				t.Fatalf("failed frame charge reused or leaked memory: %+v", action)
			}
		}
		return
	}
	t.Fatal("missing constrained invocation")
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
