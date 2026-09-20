package runtime

import (
	"encoding/json"
	"sync"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestConcurrentConstantLoadsSharePublishedBacking(t *testing.T) {
	artifact := ir.NewArtifact("example/constants", "constants")
	artifact.Constants = []ir.Constant{{ID: "bytes", Type: testType("Slice<Uint8>"), Value: json.RawMessage(`"YWJj"`)}}
	artifact.Functions = []ir.Function{{
		ID: "fn.metadata", Signature: testSignature("function()"),
		Locals:       []ir.Local{{ID: "value", Type: testType("struct{Items:Slice<Uint8>}")}},
		Instructions: []ir.Instruction{{Op: string(ir.OpReturn), Payload: json.RawMessage(`{"result_count":0}`)}},
	}}
	machine, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	module := machine.revision.Load().root
	results := make([]*vmSlice, 4)
	var group sync.WaitGroup
	for index := range results {
		group.Go(func() {
			if text := module.executable.FunctionOrder[0].LocalTypes[0].String(); text != "struct{Items:Slice<Uint8>}" {
				t.Errorf("prepared local type: %q", text)
			}
			var value vmValue
			var err error
			if index%2 == 0 {
				value, err = module.constantValueAt(0)
			} else {
				value, _, err = module.constantValue("bytes")
			}
			if err != nil {
				t.Error(err)
				return
			}
			results[index] = value.Data.(*vmSlice)
		})
	}
	group.Wait()
	for _, result := range results {
		if result == nil || result.vmSliceStorage != results[0].vmSliceStorage {
			t.Fatal("constant loads published different storage")
		}
	}
	results[0].writeBytes(0, []byte("Z"))
	for _, result := range results {
		if got := string(result.bytes()); got != "Zbc" {
			t.Fatalf("constant view lost shared mutation: %q", got)
		}
	}
}
