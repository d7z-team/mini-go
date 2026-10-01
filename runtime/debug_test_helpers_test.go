package runtime

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

var testInstructionSymbols = struct {
	sync.RWMutex
	functions map[*ir.Function][]ir.InstructionSymbol
}{functions: make(map[*ir.Function][]ir.InstructionSymbol)}

func continueTestExecution(execution *Execution) (vmResult, error) {
	return execution.resumeDebug("")
}

func stepIntoTestExecution(execution *Execution) (vmResult, error) {
	return execution.resumeDebug(debugStepInto)
}

func stepOutTestExecution(execution *Execution) (vmResult, error) {
	return execution.resumeDebug(debugStepOut)
}

type testInstructionLocation struct {
	function string
	pc       int
	file     string
	line     int
	column   int
}

func setTestInstructionLocations(t *testing.T, artifact *ir.Artifact, locations ...testInstructionLocation) {
	t.Helper()
	for _, location := range locations {
		found := false
		for functionIndex := range artifact.Functions {
			function := &artifact.Functions[functionIndex]
			if function.ID != location.function {
				continue
			}
			if location.pc < 0 || location.pc >= len(function.Code.Instructions) {
				t.Fatalf("instruction location %s pc %d is out of range", location.function, location.pc)
			}
			testInstructionSymbols.Lock()
			file := location.file
			if file == "" {
				file = "main.mgo"
			}
			testInstructionSymbols.functions[function] = append(testInstructionSymbols.functions[function], ir.InstructionSymbol{
				PC: location.pc, Points: []ir.Location{{File: file, Line: location.line, Column: location.column}},
			})
			testInstructionSymbols.Unlock()
			found = true
			break
		}
		if !found {
			t.Fatalf("instruction location references unknown function %s", location.function)
		}
	}
}

func testSymbolIndex(artifact *ir.Artifact) *symbolIndex {
	if artifact == nil {
		return nil
	}
	pkg := packageSymbolIndex{
		functions: make(map[string]ir.FunctionSymbols, len(artifact.Functions)),
		globals:   make(map[string]string, len(artifact.Globals)),
	}
	for index := range artifact.Functions {
		function := &artifact.Functions[index]
		symbols := ir.FunctionSymbols{ID: function.ID, Name: testSymbolName(function.ID)}
		for _, local := range function.Locals {
			symbols.Locals = append(symbols.Locals, ir.LocalSymbol{ID: local.ID, Name: testSymbolName(local.ID)})
		}
		for _, upvalue := range function.Upvalues {
			symbols.Upvalues = append(symbols.Upvalues, ir.UpvalueSymbol{ID: upvalue.ID, Name: testSymbolName(upvalue.ID)})
		}
		testInstructionSymbols.RLock()
		locations := append([]ir.InstructionSymbol(nil), testInstructionSymbols.functions[function]...)
		testInstructionSymbols.RUnlock()
		pcs := make([]int, len(function.Code.Instructions))
		finalPC := 0
		for instructionIndex, instruction := range function.Code.Instructions {
			pcs[instructionIndex] = finalPC
			if instruction.Op != ir.OpLabel {
				finalPC++
			}
		}
		byPC := make(map[int][]ir.Location, len(locations))
		for _, location := range locations {
			pc := pcs[location.PC]
			if pc < finalPC {
				byPC[pc] = append(byPC[pc], location.Points...)
			}
		}
		for pc, points := range byPC {
			symbols.Locations = append(symbols.Locations, ir.InstructionSymbol{PC: pc, Points: points})
		}
		sort.Slice(symbols.Locations, func(i, j int) bool { return symbols.Locations[i].PC < symbols.Locations[j].PC })
		pkg.functions[function.ID] = symbols
	}
	for _, global := range artifact.Globals {
		pkg.globals[global.ID] = testSymbolName(global.ID)
	}
	return &symbolIndex{hash: "test-symbols", packages: map[string]packageSymbolIndex{artifact.Module.Path: pkg}}
}

func testSymbolName(id string) string {
	if index := strings.LastIndexByte(id, '.'); index >= 0 && index+1 < len(id) {
		return id[index+1:]
	}
	return id
}

func setTestBreakpoints(t *testing.T, machine *vm, modulePath, file string, lines ...int) []ResolvedBreakpoint {
	t.Helper()
	resolved, err := (&Instance{vm: machine}).SetBreakpoints(modulePath, file, lines)
	if err != nil {
		t.Fatalf("set breakpoints: %v", err)
	}
	return resolved
}

func requireDebugEvent(t *testing.T, event Event, kind EventKind, functionID string, pc, line int) {
	t.Helper()
	requireDebugEventWithContext(t, event, kind, functionID, pc, line, 1)
}

func requireDebugEventWithContext(t *testing.T, event Event, kind EventKind, functionID string, pc, line int, contextID int64) {
	t.Helper()
	expect := ExpectedEvent{
		Kind:               kind,
		RunID:              1,
		ExecutionContextID: contextID,
		FunctionID:         functionID,
		PC:                 pc,
		Line:               line,
	}
	if err := expect.Match(event); err != nil {
		t.Fatalf("debug event validation failed: %v", err)
	}
	if len(event.Stack) == 0 || event.Stack[0].ExecutionContextID != event.Frame.ExecutionContextID || event.Stack[0].FunctionID != event.Frame.FunctionID || event.Stack[0].PC != event.Frame.PC {
		t.Fatalf("expected stack[0] to match current frame, got %#v", event.Stack)
	}
}

func requireDebugInt64(t *testing.T, value Value, want int64) {
	t.Helper()
	if err := value.Validate(); err != nil {
		t.Fatalf("debug value validation failed: %v", err)
	}
	if value.Kind != ValueInt64 || value.Int64 == nil || *value.Int64 != want {
		t.Fatalf("expected int64 debug value %d, got %#v", want, value)
	}
}

func requireDebugString(t *testing.T, value Value, want string) {
	t.Helper()
	if err := value.Validate(); err != nil {
		t.Fatalf("debug value validation failed: %v", err)
	}
	if value.Kind != ValueString || value.String == nil || *value.String != want {
		t.Fatalf("expected string debug value %q, got %#v", want, value)
	}
}

// deferredPanicTestArtifact runs cleanup while unwinding the main frame's panic.
func deferredPanicTestArtifact(cleanup *ir.SlotCode) ir.Artifact {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Constants = []ir.Constant{{ID: "c.message", Type: testType("String"), Value: json.RawMessage(`"failed"`)}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"function() Void", "String"}, []ir.Instruction{
			{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "fn.cleanup"}},
			{Op: ir.OpDeferPush, Payload: ir.DeferPayload{}},
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "c.message"}},
			{Op: ir.OpPanic},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}, {
		ID:        "fn.cleanup",
		Signature: testSignature("function() Void"),
		Code:      cleanup,
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	return artifact
}
