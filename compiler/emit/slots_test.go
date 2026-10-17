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
	for _, protected := range []string{"", "address", "capture", "rebind", "source boundary", "second use"} {
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
			case "rebind":
				code.Descriptors.Local[0].Rebind = true
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

func TestDirectLocalResultsForwardsSingleDirectCall(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{
		Locals: []hir.Local{{ID: "sum", Type: integer}},
	}
	code := ir.SlotCode{
		Types: []types.TypeRef{integer},
		Operands: []ir.SlotOperands{
			{Outputs: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}},
		},
	}
	callDescriptor, err := code.Descriptors.Append(ir.CallPayload{Function: "fn.update", ResultCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	localDescriptor, err := code.Descriptors.Append(ir.LocalPayload{Local: "sum"})
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []ir.SlotInstruction{
		{Op: ir.OpCallDirect, Descriptor: callDescriptor},
		{Op: ir.OpStoreLocal, Descriptor: localDescriptor, Operands: 1},
	}
	if err := directLocalResults(&code, &function, &types.TypeTable{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := code.Operands[0].Outputs; len(got) != 1 || got[0] != ir.LocalOutput {
		t.Fatalf("direct call result was not forwarded to local: %v", got)
	}
	if len(code.Instructions) != 1 || code.Instructions[0].Op != ir.OpCallDirect {
		t.Fatalf("store_local was not removed: %#v", code.Instructions)
	}
	if err := ir.ValidateSlotCode(&code, nil, ir.Local{ID: "sum", Type: integer}); err != nil {
		t.Fatalf("forwarded call failed slot validation: %v", err)
	}
}

func TestDirectLocalResultsPreservesInterveningLocalRead(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Locals: []hir.Local{{ID: "a", Type: integer}, {ID: "b", Type: integer}}}
	code := ir.SlotCode{
		Types: []types.TypeRef{integer},
		Descriptors: ir.DescriptorTables{
			Operator: []ir.OperatorPayload{{Operator: "%"}},
			Local:    []ir.LocalPayload{{Local: "a"}, {Local: "b"}},
		},
		Instructions: []ir.SlotInstruction{
			{Op: ir.OpBinary},
			{Op: ir.OpStoreLocal, Operands: 1},
			{Op: ir.OpStoreLocal, Descriptor: 1, Operands: 2},
		},
		Operands: []ir.SlotOperands{
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandLocal, Index: 1}}, Outputs: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 1}}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}},
		},
	}
	if err := directLocalResults(&code, &function, &types.TypeTable{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 3 || code.Operands[0].Outputs[0] != 0 || code.Operands[1].Inputs[0].Index != 1 {
		t.Fatalf("parallel assignment lost the old value of b: %+v", code)
	}
}

func TestDirectLocalResultsKeepsNamedResultCell(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{
		Locals:       []hir.Local{{ID: "result", Type: integer}},
		ResultLocals: []string{"result"},
	}
	code := ir.SlotCode{
		Types: []types.TypeRef{integer},
		Operands: []ir.SlotOperands{
			{Outputs: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}},
		},
	}
	callDescriptor, err := code.Descriptors.Append(ir.CallPayload{Function: "fn.update", ResultCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	localDescriptor, err := code.Descriptors.Append(ir.LocalPayload{Local: "result"})
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []ir.SlotInstruction{
		{Op: ir.OpCallDirect, Descriptor: callDescriptor},
		{Op: ir.OpStoreLocal, Descriptor: localDescriptor, Operands: 1},
	}
	if err := directLocalResults(&code, &function, &types.TypeTable{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 2 || code.Operands[0].Outputs[0] != 0 {
		t.Fatalf("named result cell was bypassed: instructions=%#v operands=%#v", code.Instructions, code.Operands)
	}
}

func TestInterfaceCallResultKeepsMapIndexType(t *testing.T) {
	parsed := parser.ParseSource("typed-view", "typed-view.mgo", `package typedview
type View interface { Label() String }
type Item struct { Text String; Values []Int }
func (i Item) Label() String { return i.Text }
func Render(items []Item) map[String]Int {
	result := make(map[String]Int)
	for _, item := range items {
		var view View = item
		result[view.Label()] = len(item.Values)
	}
	return result
}
`)
	if source.HasErrors(parsed.Diagnostics) {
		t.Fatal(parsed.Diagnostics)
	}
	program, diagnostics := lower.Lower(semantic.Check(parsed.Program), lower.Options{})
	if source.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	if _, err := Lower(program); err != nil {
		t.Fatalf("interface call result rejected as map key: %v", err)
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
		artifact.Constants = append(artifact.Constants, ir.Constant{ID: constant.ID, Type: constant.Type, Value: constant.Value.Encode(&artifact.TypeTable, constant.Type, constant.Untyped), Untyped: constant.Untyped})
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
		name                                                  string
		sourceLocal, multipleWrites, branch, returned, rebind bool
		pointer                                               bool
	}{
		{name: "temporary"},
		{name: "pointer temporary", pointer: true},
		{name: "source local", sourceLocal: true},
		{name: "pointer source local", sourceLocal: true, pointer: true},
		{name: "reassigned temporary", multipleWrites: true},
		{name: "reassigned pointer", multipleWrites: true, pointer: true},
		{name: "rebound temporary", rebind: true},
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
			appendOperation(ir.OpStoreLocal, ir.LocalPayload{Local: "value", Rebind: test.rebind}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}})
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
			} else if test.returned {
				if last.Inputs[0].Kind != ir.OperandSlot {
					t.Fatalf("observable cell was bypassed: %+v", last)
				}
			} else if test.rebind {
				if last.Inputs[0].Kind != ir.OperandLocal {
					t.Fatalf("private read did not forward through rebound cell: %+v", last)
				}
				foundRebindStore := false
				for _, instruction := range code.Instructions {
					if instruction.Op != ir.OpStoreLocal {
						continue
					}
					payload := code.Descriptors.Local[instruction.Descriptor]
					if payload.Local == "value" && payload.Rebind {
						foundRebindStore = true
						break
					}
				}
				if !foundRebindStore {
					t.Fatalf("rebind store was removed: %+v", code.Instructions)
				}
			} else if last.Inputs[0].Kind != ir.OperandLocal {
				t.Fatalf("observable local storage was bypassed: %+v", last)
			}
		})
	}
}

func TestPrivateAggregateLocalForwardingPreservesEscapeBoundaries(t *testing.T) {
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
			if boundary == "exclusive" {
				if len(function.Locals) != 0 {
					t.Fatal("promoted aggregate retained frame storage")
				}
			}
			if boundary == "exclusive" {
				if returned.Kind != ir.OperandSlot || returned.Index != 0 {
					t.Fatalf("exclusive aggregate did not retain producer slot: %+v", returned)
				}
			} else if boundary == "addressed" || boundary == "return cell" {
				if returned.Kind != ir.OperandSlot {
					t.Fatalf("%s aggregate bypassed its cell: %+v", boundary, returned)
				}
			} else if returned.Kind != ir.OperandLocal {
				t.Fatalf("private aggregate local was not forwarded across %s: %+v", boundary, returned)
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

func TestForwardLocalRetargetsLoadSourcePoint(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Locals: []hir.Local{{ID: "value", Type: integer}}}
	code := ir.SlotCode{Types: []types.TypeRef{integer, integer}}
	appendOperation := func(op ir.Opcode, payload ir.Payload, operands ir.SlotOperands) {
		descriptor, err := code.Descriptors.Append(payload)
		if err != nil {
			t.Fatal(err)
		}
		code.Instructions = append(code.Instructions, ir.SlotInstruction{Op: op, Descriptor: descriptor, Operands: uint32(len(code.Operands))})
		code.Operands = append(code.Operands, operands)
	}
	appendOperation(ir.OpZero, ir.TypePayload{Type: integer}, ir.SlotOperands{Outputs: []uint32{0}})
	appendOperation(ir.OpStoreLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}})
	appendOperation(ir.OpLoadLocal, ir.LocalPayload{Local: "value"}, ir.SlotOperands{Outputs: []uint32{1}})
	appendOperation(ir.OpReturn, ir.ReturnPayload{ResultCount: 1}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}})
	symbols := &ir.FunctionSymbols{Locations: []ir.InstructionSymbol{{PC: 2, Points: []ir.Location{{File: "main.mgo", Line: 3}}}}}
	if err := forwardLocalOperands(&code, &function, &types.TypeTable{}, symbols); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 3 || code.Instructions[2].Op != ir.OpReturn {
		t.Fatalf("forwarded source instruction = %#v", code.Instructions)
	}
	input := code.Operands[code.Instructions[2].Operands].Inputs[0]
	if input.Kind != ir.OperandLocal || input.Index != 0 {
		t.Fatalf("return input = %+v, want private local", input)
	}
	if len(symbols.Locations) != 1 || symbols.Locations[0].PC != 2 || symbols.Locations[0].Points[0].Line != 3 {
		t.Fatalf("load source point did not follow its retained instruction: %+v", symbols)
	}
}

func TestForwardLocalPreservesSnapshotAtLaterSourcePoint(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Locals: []hir.Local{{ID: "value", Type: integer}}}
	code := ir.SlotCode{
		Types: []types.TypeRef{integer},
		Descriptors: ir.DescriptorTables{
			Local:  []ir.LocalPayload{{Local: "value"}},
			Return: []ir.ReturnPayload{{ResultCount: 1}},
		},
		Instructions: []ir.SlotInstruction{{Op: ir.OpLoadLocal}, {Op: ir.OpReturn, Operands: 1}},
		Operands: []ir.SlotOperands{
			{Outputs: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}},
		},
	}
	symbols := &ir.FunctionSymbols{Locations: []ir.InstructionSymbol{{PC: 1, Points: []ir.Location{{File: "main.mgo", Line: 4}}}}}
	if err := forwardLocalOperands(&code, &function, &types.TypeTable{}, symbols); err != nil {
		t.Fatal(err)
	}
	input := code.Operands[code.Instructions[len(code.Instructions)-1].Operands].Inputs[0]
	if input.Kind != ir.OperandSlot || input.Index != 0 || symbols.Locations[0].PC != 1 {
		t.Fatalf("later observation lost the earlier value snapshot: input=%+v symbols=%+v", input, symbols)
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

func TestLowerMapRangeFusesStoredKeyAndValueAfterOkStoreElimination(t *testing.T) {
	parsed := parser.ParseSource("probe/map_range", "main.mgo", `package main
var m map[int]int
func Main(n int) int {
	if n > 0 {
		m = make(map[int]int)
		for i := 0; i < n; i++ { m[i] = i }
		return n
	}
	sum := 0
	for k, v := range m { sum += k + v }
	return sum
}`)
	if source.HasErrors(parsed.Diagnostics) {
		t.Fatal(parsed.Diagnostics)
	}
	program, diagnostics := lower.Lower(semantic.Check(parsed.Program), lower.Options{})
	if source.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	artifact, err := Lower(program)
	if err != nil {
		t.Fatal(err)
	}
	var main *ir.Function
	for index := range artifact.Functions {
		if artifact.Functions[index].ID == "fn.Main" {
			main = &artifact.Functions[index]
			break
		}
	}
	if main == nil || main.Code == nil {
		t.Fatal("compiled map probe has no Main function")
	}
	var found bool
	for _, instruction := range main.Code.Instructions {
		if instruction.Op != ir.OpMapIterNext {
			continue
		}
		outputs := main.Code.Operands[instruction.Operands].Outputs
		if len(outputs) != 3 || outputs[0]&ir.LocalOutput == 0 || outputs[1]&ir.LocalOutput == 0 || outputs[2]&ir.LocalOutput != 0 {
			t.Fatalf("map iterator outputs did not retain only the branch slot: %v", outputs)
		}
		found = true
	}
	if !found {
		t.Fatal("compiled map probe has no MapIterNext")
	}
}

func TestLowerParallelAssignmentPreservesDuplicateTargetStoreOrder(t *testing.T) {
	parsed := parser.ParseSource("parallel-assign", "main.mgo", `package main
func Main() int {
	x, y := 40, 2
	x, y = y, x
	x, y, x = y, x, x+y
	return x + y
}`)
	if source.HasErrors(parsed.Diagnostics) {
		t.Fatal(parsed.Diagnostics)
	}
	program, diagnostics := lower.Lower(semantic.Check(parsed.Program), lower.Options{})
	if source.HasErrors(diagnostics) {
		t.Fatal(diagnostics)
	}
	artifact, err := Lower(program)
	if err != nil {
		t.Fatal(err)
	}
	var main *ir.Function
	for index := range artifact.Functions {
		if artifact.Functions[index].ID == "fn.Main" {
			main = &artifact.Functions[index]
			break
		}
	}
	if main == nil || main.Code == nil {
		t.Fatal("compiled parallel assignment has no Main function")
	}
	for pc, instruction := range main.Code.Instructions {
		if instruction.Op != ir.OpBinary {
			continue
		}
		outputs := main.Code.Operands[instruction.Operands].Outputs
		if len(outputs) != 1 || outputs[0]&ir.LocalOutput == 0 {
			continue
		}
		stores := make([]string, 0, 3)
		for next := pc + 1; next < len(main.Code.Instructions) && main.Code.Instructions[next].Op == ir.OpStoreLocal; next++ {
			payload := main.Code.Descriptors.Local[main.Code.Instructions[next].Descriptor]
			stores = append(stores, payload.Local)
		}
		if len(stores) == 3 && stores[0] == stores[2] {
			t.Fatalf("binary result published before duplicate-target stores: pc=%d outputs=%v stores=%v", pc, outputs, stores)
		}
	}
}
