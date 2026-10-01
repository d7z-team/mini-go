package emit

import (
	"encoding/json"
	"testing"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/lower"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestDirectLocalResultsPreserveAddressableStorageAndBoundaries(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	for _, protected := range []string{"", "address", "capture", "source boundary", "second use"} {
		t.Run(protected, func(t *testing.T) {
			function := hir.Function{Locals: []hir.Local{{ID: "sum", Type: integer}}}
			code := ir.SlotCode{Types: []types.TypeRef{integer}, Operands: []ir.SlotOperands{
				{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandLocal}}, Outputs: []uint32{0}},
				{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}},
			}}
			code.Descriptors.Operator = []ir.OperatorPayload{{Operator: "+"}}
			code.Descriptors.Local = []ir.LocalPayload{{Local: "sum"}}
			code.Instructions = []ir.SlotInstruction{{Op: ir.OpBinary}, {Op: ir.OpStoreLocal, Operands: 1}}
			symbols := ir.FunctionSymbols{}
			switch protected {
			case "address":
				code.Descriptors.Address = []ir.AddressPayload{{Kind: "local", Local: "sum"}}
			case "capture":
				code.Descriptors.Closure = []ir.ClosurePayload{{Captures: []ir.AddressPayload{{Kind: "local", Local: "sum"}}}}
			case "source boundary":
				symbols.Locations = []ir.InstructionSymbol{{PC: 1, Points: []ir.Location{{File: "main.mgo", Line: 2}}}}
			case "second use":
				code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: ir.OpStoreLocal, Operands: 1})
			}
			if err := directLocalResults(&code, &function, &types.TypeTable{}, &symbols); err != nil {
				t.Fatal(err)
			}
			if protected != "" {
				if code.Operands[0].Outputs[0] != 0 {
					t.Fatal("fused an observable or shared store")
				}
				return
			}
			if err := allocateSlots(&code); err != nil {
				t.Fatal(err)
			}
			if err := ir.ValidateSlotCode(&code, nil, ir.Local{ID: "sum", Type: integer}); err != nil {
				t.Fatal(err)
			}
			if len(code.Types) != 0 || code.Operands[0].Outputs[0] != ir.LocalOutput {
				t.Fatal("direct result retained its temporary")
			}
		})
	}
}

func TestSlotLivenessClearsReferencesWhenBranchSkipsLastUse(t *testing.T) {
	text := types.Builtin(types.PrimitiveString)
	code := ir.SlotCode{Types: []types.TypeRef{text}, Operands: []ir.SlotOperands{
		{Outputs: []uint32{0}},
		{Inputs: []ir.Operand{{Kind: ir.OperandConstant, Index: 0}}},
		{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}},
		{},
		{Inputs: []ir.Operand{{Kind: ir.OperandConstant, Index: 1}}},
	}}
	for pc, instruction := range []ir.Instruction{
		{Op: ir.OpZero, Payload: ir.TypePayload{Type: text}},
		{Op: ir.OpJumpIf, Payload: ir.JumpPayload{Label: "join"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "join"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
	} {
		index, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			t.Fatal(err)
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: instruction.Op, Descriptor: index, Operands: uint32(pc)})
	}
	if err := allocateSlots(&code); err != nil {
		t.Fatal(err)
	}
	constants := []ir.Constant{
		{ID: "branch", Type: types.Builtin(types.PrimitiveBool), Value: json.RawMessage(`true`)},
		{ID: "result", Type: text, Value: json.RawMessage(`"done"`)},
	}
	if err := ir.ValidateSlotCode(&code, constants); err != nil {
		t.Fatal(err)
	}
	if released := code.Operands[4].ReleaseBefore; len(released) != 1 || released[0] != 0 {
		t.Fatalf("branch join retains dead string: %v", released)
	}
	if len(code.Operands[2].ReleaseBefore) != 0 {
		t.Fatal("fallthrough clears a live input")
	}
}

func TestSlotEntryReleaseTracksReferencesAcrossPredecessors(t *testing.T) {
	text := types.Builtin(types.PrimitiveString)
	code := ir.SlotCode{Types: []types.TypeRef{text, text}, Operands: []ir.SlotOperands{
		{Outputs: []uint32{0}},
		{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}},
		{},
		{},
		{Outputs: []uint32{1}},
		{Inputs: []ir.Operand{{Kind: ir.OperandConstant, Index: 0}}},
		{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}},
		{},
		{},
		{},
		{},
		{},
	}}
	for pc, instruction := range []ir.Instruction{
		{Op: ir.OpZero, Payload: ir.TypePayload{Type: text}},
		{Op: ir.OpPop},
		{Op: ir.OpJump, Payload: ir.JumpPayload{Label: "next"}},
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "next"}},
		{Op: ir.OpZero, Payload: ir.TypePayload{Type: text}},
		{Op: ir.OpJumpIf, Payload: ir.JumpPayload{Label: "skip"}},
		{Op: ir.OpPop},
		{Op: ir.OpJump, Payload: ir.JumpPayload{Label: "join"}},
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "skip"}},
		{Op: ir.OpJump, Payload: ir.JumpPayload{Label: "join"}},
		{Op: ir.OpLabel, Payload: ir.LabelPayload{Label: "join"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
	} {
		descriptor, err := code.Descriptors.Append(instruction.Payload)
		if err != nil {
			t.Fatal(err)
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: instruction.Op, Descriptor: descriptor, Operands: uint32(pc)})
	}
	if err := allocateSlots(&code); err != nil {
		t.Fatal(err)
	}
	if err := ir.ValidateSlotCode(&code, []ir.Constant{{ID: "branch", Type: types.Builtin(types.PrimitiveBool), Value: json.RawMessage(`true`)}}); err != nil {
		t.Fatal(err)
	}
	if len(code.Operands[4].ReleaseBefore) != 0 {
		t.Fatalf("entry repeats predecessor's completed release: %v", code.Operands[4].ReleaseBefore)
	}
	retained := code.Operands[4].Outputs[0]
	if released := code.Operands[9].ReleaseBefore; len(released) != 1 || released[0] != retained {
		t.Fatalf("skipped last-use path retains root %d: %v", retained, released)
	}
	if released := code.Operands[11].ReleaseBefore; len(released) != 1 || released[0] != retained {
		t.Fatalf("join fails to account for predecessor root %d: %v", retained, released)
	}
}

func TestSlotLoweringPreservesTypedOperandsAcrossControlFlow(t *testing.T) {
	parsed := parser.ParseSource("slots", "slots.mgo", `package slots
type Pair struct { A int; B int }
type Reader interface { Read() int }
func (p Pair) Read() int { return p.A }
func Change(p *int) int { *p = 99; return 2 }
func Order() int { x := 40; return x + Change(&x) }
func Logical(a, b bool) bool { return a && b || !a }
func Sum(n int) int { sum := 0; for i := 0; i < n; i++ { sum += i }; return sum }
func SelectType(x any) int { switch v := x.(type) { case int: return v; case Reader: return v.Read(); default: return 0 } }
func Aggregate() int { p := Pair{A: 40, B: 2}; a := []int{p.A, p.B}; m := map[int]int{0: a[0]}; return m[0] + a[1] }
func Iteration() int { m := map[int]int{1: 2}; sum := 0; for k,v := range m { sum += k+v }; return sum }
func Closure() int { x := 40; f := func() int { return x+2 }; return f() }
func Receive(c chan int) int { v, ok := <-c; if ok { return v }; return 0 }
func Main() int { return Order() }
`)
	if source.HasErrors(parsed.Diagnostics) {
		t.Fatal(parsed.Diagnostics)
	}
	program, diagnostics := lower.Lower(semantic.Check(parsed.Program), lower.Options{})
	if source.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	artifact := ir.NewArtifact(program.ModulePath, program.Package)
	artifact.TypeTable = types.CloneTable(program.TypeTable)
	for _, constant := range program.Constants {
		artifact.Constants = append(artifact.Constants, ir.Constant{ID: constant.ID, Type: constant.Type, Value: constant.Value, Untyped: constant.Untyped})
	}
	for _, global := range program.Globals {
		artifact.Globals = append(artifact.Globals, ir.Global{ID: global.ID, Type: global.Type})
	}
	emitter := newPackageEmitter(&artifact, program.Functions)
	for i := range program.Functions {
		fn := &program.Functions[i]
		t.Run(fn.ID, func(t *testing.T) {
			code, err := emitter.lowerFunction(fn, nil)
			if err != nil {
				t.Fatal(err)
			}
			inputTypes := make([][]types.TypeRef, len(code.Instructions))
			for pc, instruction := range code.Instructions {
				for _, input := range code.Operands[instruction.Operands].Inputs {
					if input.Kind == ir.OperandSlot {
						inputTypes[pc] = append(inputTypes[pc], code.Types[input.Index])
					}
				}
			}
			if err := allocateSlots(&code); err != nil {
				t.Fatal(err)
			}
			if err := ir.ValidateSlotCode(&code, artifact.Constants); err != nil {
				t.Fatal(err)
			}
			for pc, instruction := range code.Instructions {
				index := 0
				for _, input := range code.Operands[instruction.Operands].Inputs {
					if input.Kind != ir.OperandSlot {
						continue
					}
					if actual := code.Types[input.Index]; actual != inputTypes[pc][index] || !actual.Valid() {
						t.Fatalf("operand type changed at %d: %v -> %v", pc, inputTypes[pc][index], actual)
					}
					index++
				}
			}
		})
	}
}

func TestTemporaryPromotionPreservesStorageBoundaries(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	for _, test := range []struct {
		name                                          string
		sourceLocal, multipleWrites, branch, returned bool
		pointer                                       bool
	}{
		{name: "temporary"},
		{name: "pointer temporary", pointer: true},
		{name: "source local", sourceLocal: true},
		{name: "pointer source local", sourceLocal: true, pointer: true},
		{name: "reassigned temporary", multipleWrites: true},
		{name: "reassigned pointer", multipleWrites: true, pointer: true},
		{name: "branch crossing", branch: true},
		{name: "return cell", returned: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := types.TypeTable{Nodes: []types.TypeNode{{ID: "pointer", Kind: types.Pointer, Elem: integer}}}
			valueType := integer
			if test.pointer {
				valueType = types.TypeRef{Kind: types.Pointer, Node: "pointer"}
			}
			function := hir.Function{Locals: []hir.Local{{ID: "value", Type: valueType, Generated: !test.sourceLocal}}}
			if test.returned {
				function.ResultLocals = []string{"value"}
			}
			code := ir.SlotCode{Types: []types.TypeRef{valueType, valueType}}
			appendOperation := func(op ir.Opcode, payload ir.Payload, operands ir.SlotOperands) {
				descriptor, err := code.Descriptors.Append(payload)
				if err != nil {
					t.Fatal(err)
				}
				code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: op, Descriptor: descriptor, Operands: uint32(len(code.Operands))})
				code.Operands = append(code.Operands, operands)
			}
			appendOperation(ir.OpZero, ir.TypePayload{Type: valueType}, ir.SlotOperands{Outputs: []uint32{0}})
			appendOperation(ir.OpStoreLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}})
			if test.multipleWrites {
				appendOperation(ir.OpStoreLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}})
			}
			if test.branch {
				appendOperation(ir.OpLabel, ir.LabelPayload{Label: "join"}, ir.SlotOperands{})
			}
			appendOperation(ir.OpLoadLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Outputs: []uint32{1}})
			appendOperation(ir.OpReturn, ir.ReturnPayload{ResultCount: 1}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}})
			if err := forwardLocalOperands(&code, &function, &table, nil); err != nil {
				t.Fatal(err)
			}
			if err := allocateSlots(&code); err != nil {
				t.Fatal(err)
			}
			if err := ir.ValidateSlotCode(&code, nil, lowerLocals(function.Locals)...); err != nil {
				t.Fatal(err)
			}
			last := code.Operands[code.Instructions[len(code.Instructions)-1].Operands]
			if test.name == "temporary" || test.name == "pointer temporary" {
				if len(function.Locals) != 0 {
					t.Fatal("promoted temporary retained frame storage")
				}
				if len(code.Instructions) != 2 || last.Inputs[0].Kind != ir.OperandSlot || last.Inputs[0].Index != code.Operands[code.Instructions[0].Operands].Outputs[0] {
					t.Fatalf("temporary did not preserve the producer slot: %+v", code)
				}
			} else if last.Inputs[0].Kind != ir.OperandLocal {
				t.Fatalf("observable local storage was bypassed: %+v", last)
			}
		})
	}
}

func TestAggregateTemporaryPromotionRequiresExclusiveConstruction(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	array := types.TypeRef{Kind: types.Array, Node: "array"}
	pointer := types.TypeRef{Kind: types.Pointer, Node: "pointer"}
	table := types.TypeTable{Nodes: []types.TypeNode{{ID: "array", Kind: types.Array, Elem: integer, Length: 1}, {ID: "pointer", Kind: types.Pointer, Elem: array}}}
	for _, boundary := range []string{"exclusive", "other producer", "other consumer", "addressed", "source local", "branch", "return cell"} {
		t.Run(boundary, func(t *testing.T) {
			function := hir.Function{Locals: []hir.Local{{ID: "value", Type: array, Generated: boundary != "source local"}}}
			if boundary == "return cell" {
				function.ResultLocals = []string{"value"}
			}
			code := ir.SlotCode{Types: []types.TypeRef{array, array, integer, pointer}}
			appendOperation := func(op ir.Opcode, payload ir.Payload, operands ir.SlotOperands) {
				descriptor, err := code.Descriptors.Append(payload)
				if err != nil {
					t.Fatal(err)
				}
				code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: op, Descriptor: descriptor, Operands: uint32(len(code.Operands))})
				code.Operands = append(code.Operands, operands)
			}
			appendOperation(ir.OpZero, ir.TypePayload{Type: integer}, ir.SlotOperands{Outputs: []uint32{2}})
			if boundary == "other producer" {
				appendOperation(ir.OpZero, ir.TypePayload{Type: array}, ir.SlotOperands{Outputs: []uint32{0}})
			} else {
				appendOperation(ir.OpMakeSequence, ir.MakeSequencePayload{Type: array, ElementCount: 1}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}, Outputs: []uint32{0}})
			}
			appendOperation(ir.OpStoreLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}})
			if boundary == "other consumer" {
				appendOperation(ir.OpPop, nil, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}})
			}
			if boundary == "addressed" {
				appendOperation(ir.OpAddressOf, ir.AddressPayload{Kind: "local", Local: "value"}, ir.SlotOperands{Outputs: []uint32{3}})
			}
			if boundary == "branch" {
				appendOperation(ir.OpLabel, ir.LabelPayload{Label: "join"}, ir.SlotOperands{})
			}
			appendOperation(ir.OpLoadLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Outputs: []uint32{1}})
			appendOperation(ir.OpReturn, ir.ReturnPayload{ResultCount: 1}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}})
			if err := forwardLocalOperands(&code, &function, &table, nil); err != nil {
				t.Fatal(err)
			}
			returned := code.Operands[code.Instructions[len(code.Instructions)-1].Operands].Inputs[0]
			want := uint32(1)
			if boundary == "exclusive" {
				want = 0
				if len(function.Locals) != 0 {
					t.Fatal("promoted aggregate retained frame storage")
				}
			}
			if returned.Kind != ir.OperandSlot || returned.Index != want {
				t.Fatalf("aggregate ownership changed across %s: %+v", boundary, returned)
			}
			if err := allocateSlots(&code); err != nil {
				t.Fatal(err)
			}
			if err := ir.ValidateSlotCode(&code, nil, lowerLocals(function.Locals)...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTemporaryLocalCompactionRetainsImplicitStorageAndRemapsDirectOperands(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Signature: types.FunctionSignature{Params: []types.TypeParam{{Type: integer}}}, ResultLocals: []string{"result"}}
	for _, name := range []string{"argument", "unused", "source", "result", "address", "index", "capture", "subject", "binding", "channel", "send", "received", "ok", "selected"} {
		function.Locals = append(function.Locals, hir.Local{ID: name, Type: integer, Generated: name != "source"})
	}
	original := function.Locals
	symbols := ir.FunctionSymbols{Locals: lowerLocalSymbols(function.Locals)}
	code := ir.SlotCode{
		Operands: []ir.SlotOperands{{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 2}}}},
		Descriptors: ir.DescriptorTables{
			Address:      []ir.AddressPayload{{Kind: "local", Local: "address", Path: []ir.AddressPathSegment{{Local: "index"}}}},
			Closure:      []ir.ClosurePayload{{Captures: []ir.AddressPayload{{Kind: "local", Local: "capture"}}}},
			TypeDispatch: []ir.TypeDispatchPayload{{Subject: "subject", Cases: []ir.TypeCase{{Binding: "binding"}}}},
			Select:       []ir.SelectPayload{{Index: "selected", Cases: []ir.SelectCase{{Channel: "channel", Send: "send", Value: "received", OK: "ok"}}}},
		},
	}
	pruneUnusedTemporaryLocals(&code, &function, &symbols)
	if len(function.Locals) != len(original)-1 || len(symbols.Locals) != len(function.Locals) {
		t.Fatalf("frame or symbol layout mismatch: %+v / %+v", function.Locals, symbols.Locals)
	}
	for index, local := range function.Locals {
		originalIndex := index
		if index != 0 {
			originalIndex++
		}
		if local.ID != original[originalIndex].ID || symbols.Locals[index].ID != local.ID {
			t.Fatalf("implicit cell or symbol was removed: %+v / %+v", function.Locals, symbols.Locals)
		}
	}
	if original[1].ID != "unused" {
		t.Fatal("compaction mutated the source HIR storage")
	}
	if input := code.Operands[0].Inputs[0]; input.Index != 1 || function.Locals[input.Index].ID != "source" {
		t.Fatalf("direct operand refers to a different cell: %+v", input)
	}
}
