package runtime

import (
	"context"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func FuzzDebugVariables(f *testing.F) {
	f.Add(0, 0, 1, true)
	f.Add(1, 1, 1, true)
	f.Add(-1, 1, 1, true)
	f.Fuzz(func(t *testing.T, start, count, reference int, paused bool) {
		state := ExecutionRunning
		if paused {
			state = ExecutionPaused
		}
		execution := &Execution{
			state: state,
			debugInspection: &debugInspection{references: map[int]debugReference{1: {bindings: []debugLocal{
				{Name: "first", Type: "Int", Value: newVMValue("Int", int64(1))},
				{Name: "second", Type: "Int", Value: newVMValue("Int", int64(2))},
			}}}},
		}
		variables, err := execution.DebugVariables(reference, start, count)
		if !paused || reference != 1 || start < 0 || count < 0 {
			if err == nil {
				t.Fatal("invalid debug variable request succeeded")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(variables) > 2 {
			t.Fatalf("returned %d variables", len(variables))
		}
	})
}

func FuzzDebugInspectionLifecycle(f *testing.F) {
	f.Add([]byte{0, 2, 0, 1, 2, 3, 0})
	f.Add([]byte{1, 0, 2, 0})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 1<<10 {
			return
		}
		reference := 1
		execution := &Execution{}
		pause := func() {
			execution.state = ExecutionPaused
			execution.debugInspection = &debugInspection{references: map[int]debugReference{
				reference: {bindings: []debugLocal{{Name: "value", Type: "Int", Value: newVMValue("Int", int64(reference))}}},
			}}
		}
		pause()
		for _, operation := range operations {
			switch operation % 4 {
			case 0:
				variables, err := execution.DebugVariables(reference, 0, 1)
				if execution.state == ExecutionPaused {
					if err != nil || len(variables) != 1 {
						t.Fatalf("current reference: variables=%#v err=%v", variables, err)
					}
				} else if err == nil {
					t.Fatal("reference remained valid outside a pause")
				}
			case 1:
				execution.state = ExecutionRunning
				execution.debugInspection = nil
			case 2:
				stale := reference
				reference++
				pause()
				if _, err := execution.DebugVariables(stale, 0, 1); err == nil {
					t.Fatal("reference from an earlier pause remained valid")
				}
			case 3:
				if _, err := execution.DebugVariables(reference, -1, 1); err == nil {
					t.Fatal("negative variable range succeeded")
				}
			}
		}
	})
}

func FuzzDebuggerBreakpointUpdates(f *testing.F) {
	f.Add([]byte{1, 4, 9}, false)
	f.Add([]byte{2, 7}, true)
	f.Fuzz(func(t *testing.T, encoded []byte, invalid bool) {
		if len(encoded) > 1<<10 {
			return
		}
		debugger := NewDebugger()
		artifact := ir.NewArtifact("fuzz/main", "main")
		instructions := make([]ir.Instruction, 0, 34)
		var operands [][2][]uint32
		locations := make([]testInstructionLocation, 0, 33)
		for line := 1; line <= 32; line++ {
			instructions = append(instructions,
				ir.Instruction{Op: ir.OpZero, Payload: testTypePayload("Bool")},
				ir.Instruction{Op: ir.OpPop},
			)
			locations = append(locations, testInstructionLocation{function: "fn.entry", pc: len(instructions) - 2, line: line, column: 1})
			operands = append(operands, [2][]uint32{nil, {0}}, [2][]uint32{{0}, nil})
		}
		instructions = append(instructions,
			ir.Instruction{Op: ir.OpZero, Payload: testTypePayload("Bool")},
			ir.Instruction{Op: ir.OpPop},
			ir.Instruction{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
		)
		operands = append(operands, [2][]uint32{nil, {0}}, [2][]uint32{{0}, nil}, [2][]uint32{})
		artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"Bool"}, instructions, operands)}}
		artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
		locations = append(locations, testInstructionLocation{function: "fn.entry", pc: len(instructions) - 3, file: "other.mgo", line: 5, column: 1})
		setTestInstructionLocations(t, &artifact, locations...)
		instance, err := patchTestProgram(t, artifact, "fuzz-breakpoints").Instantiate(context.Background(), InstanceOptions{Debugger: debugger})
		if err != nil {
			t.Fatal(err)
		}
		cleanupTestInstance(t, instance)
		if _, err := instance.SetBreakpoints("fuzz/main", "main.mgo", []int{3}); err != nil {
			t.Fatal(err)
		}
		if _, err := instance.SetBreakpoints("fuzz/main", "other.mgo", []int{5}); err != nil {
			t.Fatal(err)
		}
		lines := make([]int, len(encoded))
		want := make(map[int]bool, len(encoded))
		for index, value := range encoded {
			lines[index] = int(value%32) + 1
			want[lines[index]] = true
		}
		if invalid {
			lines = append(lines, 0)
		}
		_, err = instance.SetBreakpoints("fuzz/main", "main.mgo", lines)
		if invalid {
			if err == nil || !debugger.hasBreakpoint(instance.Revision().Generation, "fuzz/main", "main.mgo", 3) {
				t.Fatalf("invalid update was not atomic: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			debugger.mu.RLock()
			for line := range debugger.breakpoints[breakpointSource{ModulePath: "fuzz/main", File: "main.mgo"}].active {
				if !want[line] {
					t.Fatalf("unexpected active breakpoint at %d", line)
				}
			}
			debugger.mu.RUnlock()
			for line := range want {
				if !debugger.hasBreakpoint(instance.Revision().Generation, "fuzz/main", "main.mgo", line) {
					t.Fatalf("missing active breakpoint at %d", line)
				}
			}
		}
		if !debugger.hasBreakpoint(instance.Revision().Generation, "fuzz/main", "other.mgo", 5) {
			t.Fatal("source update changed another source")
		}
	})
}
