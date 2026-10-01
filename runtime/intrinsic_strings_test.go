package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestBoundedByteSearchValidatesRanges(t *testing.T) {
	for _, tc := range []struct {
		start, end int64
		want       int64
		invalid    bool
	}{{0, 3, 2, false}, {3, 3, -1, false}, {-1, 2, 0, true}, {2, 1, 0, true}, {0, 4, 0, true}} {
		got, err := stringsIndexByte(intrinsicContext{}, []vmValue{newVMValue("String", "a\x00b"), newVMValue("Int", tc.start), newVMValue("Int", tc.end), newVMValue("Uint8", uint64('b'))})
		if tc.invalid {
			if err == nil {
				t.Fatal("invalid range accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		offset, err := asInt64(got[0])
		if err != nil || offset != tc.want {
			t.Fatalf("offset %d, %v", offset, err)
		}
	}
}

func TestBoundedByteSearchYieldsBetweenChunks(t *testing.T) {
	artifact := ir.NewArtifact("strings/search-budget", "main")
	text, _ := json.Marshal(strings.Repeat("a", 4096))
	artifact.Constants = []ir.Constant{
		{ID: "text", Type: testType("String"), Value: text},
		{ID: "start", Type: testType("Int"), Value: []byte(`0`)},
		{ID: "end", Type: testType("Int"), Value: []byte(`4096`)},
		{ID: "needle", Type: testType("Uint8"), Value: []byte(`122`)},
	}
	code := testSlotCode([]string{"String", "Int", "Int", "Uint8", "Int"}, nil, nil)
	artifact.Functions = []ir.Function{{Code: code, ID: "fn.entry", Signature: testSignature("function() Void")}}
	for range 2 {
		for slot, id := range []string{"text", "start", "end", "needle"} {
			appendTestSlotCode(code, []ir.Instruction{{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: id}}}, [][2][]uint32{{nil, {uint32(slot)}}})
		}
		appendTestSlotCode(code, []ir.Instruction{
			{Op: ir.OpCallIntrinsic, Payload: ir.CallIntrinsicPayload{ID: "strings.index_byte", ArgCount: 4, ResultCount: 1}},
			{Op: ir.OpPop},
		}, [][2][]uint32{{{0, 1, 2, 3}, {4}}, {{4}, nil}})
	}
	appendTestSlotCode(code, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}})
	program := patchTestProgram(t, artifact, "search-budget")
	for _, cancel := range []bool{false, true} {
		instance, err := program.Instantiate(t.Context(), InstanceOptions{Limits: Limits{MaxSteps: 6}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = instance.Close() })
		execution, err := instance.Start("run")
		if err != nil {
			t.Fatal(err)
		}
		if state, steps, err := execution.PollSteps(6); err != nil || steps != 6 || state != ExecutionRunning {
			t.Fatalf("first chunk: state=%s, steps=%d, error=%v", state, steps, err)
		}
		if cancel {
			execution.Cancel()
			if execution.State() == ExecutionRunning {
				t.Fatal("cancellation left search running")
			}
		} else {
			state, steps, err := execution.PollSteps(1)
			var limit StepLimitError
			if state != ExecutionFailed || steps != 0 || !errors.As(err, &limit) {
				t.Fatalf("second chunk: state=%s, steps=%d, error=%v", state, steps, err)
			}
		}
		if err := instance.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBoundedByteSearchPreservesBytesAndEnforcesChunkLimit(t *testing.T) {
	text := strings.Repeat("x", 4096) + "\x00\xff"
	for _, tc := range []struct {
		start, end, needle, want int64
		invalid                  bool
	}{
		{0, 4096, 255, -1, false},
		{4096, 4098, 255, 4097, false},
		{4096, 4098, 0, 4096, false},
		{0, 4097, 255, 0, true},
		{0, 1, 256, 0, true},
		{0, 1, -1, 0, true},
	} {
		got, err := stringsIndexByte(intrinsicContext{}, []vmValue{newVMValue("String", text), newVMValue("Int", tc.start), newVMValue("Int", tc.end), newVMValue("Int", tc.needle)})
		if tc.invalid {
			if err == nil {
				t.Fatal("invalid search accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		offset, err := asInt64(got[0])
		if err != nil || offset != tc.want {
			t.Fatalf("offset %d, want %d: %v", offset, tc.want, err)
		}
	}
}
