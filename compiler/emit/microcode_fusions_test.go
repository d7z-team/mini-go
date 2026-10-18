package emit

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestFuseIndexConvertKeepsScalarResultAndSourceBoundary(t *testing.T) {
	stringType := types.Builtin(types.PrimitiveString)
	integer := types.Builtin(types.PrimitiveInt)
	byteType := types.Builtin(types.PrimitiveUint8)
	code := ir.MicroCode{
		Types: []types.TypeRef{stringType, integer, byteType, integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}, {Kind: ir.OperandSlot, Index: 1}}, Outputs: []uint32{2}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}, Outputs: []uint32{3}},
		},
	}
	convert, err := code.Descriptors.Append(ir.TypePayload{Type: integer})
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []ir.MicroInstruction{{Op: ir.OpLoadIndex, Operands: 0}, {Op: ir.OpConvert, Descriptor: convert, Operands: 1}}
	function := hir.Function{}
	producerSymbols := &ir.FunctionSymbols{Locations: []ir.InstructionSymbol{{PC: 0, Points: []ir.Location{{File: "main.mgo", Line: 1}}}}}
	if err := fuseIndexConvert(&code, &function, &types.TypeTable{}, nil, producerSymbols); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 1 || code.Instructions[0].Op != ir.OpIndexConvert {
		t.Fatalf("index/convert pair was not fused: %#v", code.Instructions)
	}
	operands := code.Operands[code.Instructions[0].Operands]
	if len(operands.Inputs) != 2 || len(operands.Outputs) != 1 || operands.Outputs[0] != 3 {
		t.Fatalf("fused operands changed result shape: %#v", operands)
	}
	if producerSymbols.Locations[0].PC != 0 || producerSymbols.Locations[0].Points[0].Line != 1 {
		t.Fatalf("fusion lost the producer source point: %#v", producerSymbols)
	}

	code = ir.MicroCode{
		Types: []types.TypeRef{stringType, integer, byteType, integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}, {Kind: ir.OperandSlot, Index: 1}}, Outputs: []uint32{2}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}, Outputs: []uint32{3}},
		},
	}
	convert, _ = code.Descriptors.Append(ir.TypePayload{Type: integer})
	code.Instructions = []ir.MicroInstruction{{Op: ir.OpLoadIndex, Operands: 0}, {Op: ir.OpConvert, Descriptor: convert, Operands: 1}}
	symbols := &ir.FunctionSymbols{Locations: []ir.InstructionSymbol{{PC: 1, Points: []ir.Location{{File: "main.mgo", Line: 1}}}}}
	if err := fuseIndexConvert(&code, &function, &types.TypeTable{}, nil, symbols); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 2 || code.Instructions[0].Op == ir.OpIndexConvert {
		t.Fatal("source boundary allowed index/convert fusion")
	}
}

func TestFuseLenCompareBranchNormalizesRightHandLength(t *testing.T) {
	stringType := types.Builtin(types.PrimitiveString)
	integer := types.Builtin(types.PrimitiveInt)
	code := ir.MicroCode{
		Types: []types.TypeRef{stringType, integer, integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}, Outputs: []uint32{1}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}, {Kind: ir.OperandSlot, Index: 1}}},
		},
	}
	branch, err := code.Descriptors.Append(ir.CompareBranchPayload{Operator: "<", Type: integer, Label: "done", When: true})
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []ir.MicroInstruction{{Op: ir.OpLen, Operands: 0}, {Op: ir.OpCompareBranch, Descriptor: branch, Operands: 1}}
	if err := fuseLenCompareBranch(&code, &hir.Function{}, &types.TypeTable{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 1 || code.Instructions[0].Op != ir.OpLenCompareBranch {
		t.Fatalf("len/compare pair was not fused: %#v", code.Instructions)
	}
	if got := code.Descriptors.CompareBranch[code.Instructions[0].Descriptor].Operator; got != ">" {
		t.Fatalf("right-hand length was not normalized: %q", got)
	}
}

func TestFuseUpdateCompareBranchKeepsLogicalCostAndDirectLocal(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Locals: []hir.Local{{ID: "counter", Type: integer}}}
	code := ir.MicroCode{
		Types: []types.TypeRef{integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant}}, Outputs: []uint32{ir.LocalOutput}},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant, Index: 1}}},
		},
	}
	update, _ := code.Descriptors.Append(ir.OperatorPayload{Operator: "+"})
	branch, _ := code.Descriptors.Append(ir.CompareBranchPayload{Operator: "<", Type: integer, Label: "loop", When: true})
	code.Instructions = []ir.MicroInstruction{
		{Op: ir.OpBinary, Descriptor: update, Operands: 0, Cost: 1},
		{Op: ir.OpCompareBranch, Descriptor: branch, Operands: 1, Cost: 1},
	}
	constants := []ir.Constant{{Type: integer}, {Type: integer}}
	if err := fuseUpdateCompareBranch(&code, &function, &types.TypeTable{}, constants, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 1 || code.Instructions[0].Op != ir.OpUpdateCompareBranch || code.Instructions[0].LogicalCost() != 2 {
		t.Fatalf("update/compare pair was not fused: %#v", code.Instructions)
	}
	flow := code.Operands[code.Instructions[0].Operands]
	if len(flow.Inputs) != 3 || flow.Inputs[0].Kind != ir.OperandLocal || flow.Inputs[0].Index != 0 {
		t.Fatalf("fused operands lost direct local identity: %#v", flow)
	}
}

func TestFuseLoopLatchSkipsRepeatedHeaderDispatch(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Locals: []hir.Local{{ID: "counter", Type: integer}, {ID: "value", Type: integer}}}
	code := ir.MicroCode{
		Types: []types.TypeRef{integer},
		Operands: []ir.RegisterFlow{
			{},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant, Index: 1}}},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal, Index: 1}, {Kind: ir.OperandConstant, Index: 2}}, Outputs: []uint32{ir.LocalOutput | 1}},
			{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant, Index: 2}}, Outputs: []uint32{ir.LocalOutput}},
			{},
			{},
		},
	}
	header, _ := code.Descriptors.Append(ir.LabelPayload{Label: "condition"})
	comparison, _ := code.Descriptors.Append(ir.CompareBranchPayload{Operator: "<", Type: integer, Label: "done", When: false})
	bodyUpdate, _ := code.Descriptors.Append(ir.OperatorPayload{Operator: "+"})
	counterUpdate, _ := code.Descriptors.Append(ir.OperatorPayload{Operator: "+"})
	backedge, _ := code.Descriptors.Append(ir.JumpPayload{Label: "condition"})
	done, _ := code.Descriptors.Append(ir.LabelPayload{Label: "done"})
	code.Instructions = []ir.MicroInstruction{
		{Op: ir.OpLabel, Descriptor: header, Operands: 0, Cost: 1},
		{Op: ir.OpCompareBranch, Descriptor: comparison, Operands: 1, Cost: 1},
		{Op: ir.OpBinary, Descriptor: bodyUpdate, Operands: 2, Cost: 1},
		{Op: ir.OpBinary, Descriptor: counterUpdate, Operands: 3, Cost: 1},
		{Op: ir.OpJump, Descriptor: backedge, Operands: 4, Cost: 1},
		{Op: ir.OpLabel, Descriptor: done, Operands: 5, Cost: 1},
	}
	constants := []ir.Constant{{Type: integer}, {Type: integer}, {Type: integer}}
	if err := fuseLoopLatch(&code, &function, &types.TypeTable{}, constants, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 6 {
		t.Fatalf("expected inserted body label and removed back-edge jump, got %#v", code.Instructions)
	}
	if code.Instructions[4].Op != ir.OpUpdateCompareBranch || code.Instructions[4].LogicalCost() != 3 {
		t.Fatalf("loop latch was not fused with preserved fuel: %#v", code.Instructions)
	}
	payload, err := code.Descriptors.Payload(code.Instructions[4].Op, code.Instructions[4].Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	fused := payload.(ir.UpdateCompareBranchPayload)
	if fused.Label == "condition" || !fused.When {
		t.Fatalf("fused latch does not branch directly to the body: %#v", fused)
	}
	flow := code.Operands[code.Instructions[4].Operands]
	if len(flow.Inputs) != 3 || flow.Inputs[0].Kind != ir.OperandLocal || flow.Inputs[0].Index != 0 {
		t.Fatalf("fused latch lost its direct counter local: %#v", flow)
	}
}

func TestFuseLoopLatchRequiresExitAtFallthrough(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	function := hir.Function{Locals: []hir.Local{{ID: "counter", Type: integer}, {ID: "unrelated", Type: integer}}}
	code := ir.MicroCode{Operands: []ir.RegisterFlow{
		{},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant, Index: 1}}},
		{Inputs: []ir.Operand{{Kind: ir.OperandLocal}, {Kind: ir.OperandConstant}}, Outputs: []uint32{ir.LocalOutput}},
		{},
		{Outputs: []uint32{ir.LocalOutput | 1}},
		{},
	}}
	header, _ := code.Descriptors.Append(ir.LabelPayload{Label: "condition"})
	comparison, _ := code.Descriptors.Append(ir.CompareBranchPayload{Operator: "<", Type: integer, Label: "shared-exit", When: false})
	update, _ := code.Descriptors.Append(ir.OperatorPayload{Operator: "+"})
	backedge, _ := code.Descriptors.Append(ir.JumpPayload{Label: "condition"})
	zero, _ := code.Descriptors.Append(ir.TypePayload{Type: integer})
	exit, _ := code.Descriptors.Append(ir.LabelPayload{Label: "shared-exit"})
	code.Instructions = []ir.MicroInstruction{
		{Op: ir.OpLabel, Descriptor: header, Operands: 0, Cost: 1},
		{Op: ir.OpCompareBranch, Descriptor: comparison, Operands: 1, Cost: 1},
		{Op: ir.OpBinary, Descriptor: update, Operands: 2, Cost: 1},
		{Op: ir.OpJump, Descriptor: backedge, Operands: 3, Cost: 1},
		{Op: ir.OpZero, Descriptor: zero, Operands: 4, Cost: 1},
		{Op: ir.OpLabel, Descriptor: exit, Operands: 5, Cost: 1},
	}
	constants := []ir.Constant{{Type: integer}, {Type: integer}}
	if err := fuseLoopLatch(&code, &function, &types.TypeTable{}, constants, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 6 || code.Instructions[2].Op != ir.OpBinary || code.Instructions[3].Op != ir.OpJump {
		t.Fatalf("fused latch whose false edge skips executable code: %#v", code.Instructions)
	}
}

func TestFusionRequiresOneConsumerAndNoControlBoundary(t *testing.T) {
	stringType := types.Builtin(types.PrimitiveString)
	integer := types.Builtin(types.PrimitiveInt)
	byteType := types.Builtin(types.PrimitiveUint8)
	newPair := func(extra ir.RegisterFlow) (ir.MicroCode, uint32) {
		code := ir.MicroCode{
			Types: []types.TypeRef{stringType, integer, byteType, integer},
			Operands: []ir.RegisterFlow{
				{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}, {Kind: ir.OperandSlot, Index: 1}}, Outputs: []uint32{2}},
				{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}, Outputs: []uint32{3}},
			},
		}
		convert, _ := code.Descriptors.Append(ir.TypePayload{Type: integer})
		code.Instructions = []ir.MicroInstruction{{Op: ir.OpLoadIndex, Operands: 0}, {Op: ir.OpConvert, Descriptor: convert, Operands: 1}}
		if extra.Inputs != nil || extra.Outputs != nil {
			code.Operands = append(code.Operands, extra)
			code.Instructions = append(code.Instructions, ir.MicroInstruction{Op: ir.OpPop, Operands: 2})
		}
		return code, convert
	}

	code, _ := newPair(ir.RegisterFlow{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}})
	if err := fuseIndexConvert(&code, &hir.Function{}, &types.TypeTable{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if code.Instructions[0].Op == ir.OpIndexConvert {
		t.Fatal("fused a load result with multiple consumers")
	}

	code, convert := newPair(ir.RegisterFlow{})
	label, _ := code.Descriptors.Append(ir.LabelPayload{Label: "join"})
	code.Instructions[1] = ir.MicroInstruction{Op: ir.OpLabel, Descriptor: label, Operands: 1}
	code.Instructions = append(code.Instructions, ir.MicroInstruction{Op: ir.OpConvert, Descriptor: convert, Operands: 1})
	if err := fuseIndexConvert(&code, &hir.Function{}, &types.TypeTable{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, instruction := range code.Instructions {
		if instruction.Op == ir.OpIndexConvert {
			t.Fatal("fused across a control label")
		}
	}
}

func TestFusionMergesReleaseBeforeAndHonorsScopeBoundary(t *testing.T) {
	stringType := types.Builtin(types.PrimitiveString)
	integer := types.Builtin(types.PrimitiveInt)
	byteType := types.Builtin(types.PrimitiveUint8)
	code := ir.MicroCode{
		Types: []types.TypeRef{stringType, integer, byteType, integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}, {Kind: ir.OperandSlot, Index: 1}}, Outputs: []uint32{2}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}, Outputs: []uint32{3}, ReleaseBefore: []uint32{2}},
		},
	}
	convert, _ := code.Descriptors.Append(ir.TypePayload{Type: integer})
	code.Instructions = []ir.MicroInstruction{{Op: ir.OpLoadIndex, Operands: 0}, {Op: ir.OpConvert, Descriptor: convert, Operands: 1}}
	if err := fuseIndexConvert(&code, &hir.Function{}, &types.TypeTable{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 1 || len(code.Operands[0].ReleaseBefore) != 0 {
		t.Fatalf("intermediate release was retained: %#v", code.Operands)
	}

	code = ir.MicroCode{
		Types: []types.TypeRef{stringType, integer, byteType, integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}, {Kind: ir.OperandSlot, Index: 1}}, Outputs: []uint32{2}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}, Outputs: []uint32{3}},
		},
	}
	convert, _ = code.Descriptors.Append(ir.TypePayload{Type: integer})
	code.Instructions = []ir.MicroInstruction{{Op: ir.OpLoadIndex, Operands: 0}, {Op: ir.OpConvert, Descriptor: convert, Operands: 1}}
	symbols := &ir.FunctionSymbols{Scopes: []ir.DebugScope{{Ranges: []ir.PCRange{{Start: 1, End: 2}}}}}
	if err := fuseIndexConvert(&code, &hir.Function{}, &types.TypeTable{}, nil, symbols); err != nil {
		t.Fatal(err)
	}
	if code.Instructions[0].Op == ir.OpIndexConvert {
		t.Fatal("fused across a scope boundary")
	}
}

func TestFusionAcceptsTypedConstantOperands(t *testing.T) {
	stringType := types.Builtin(types.PrimitiveString)
	integer := types.Builtin(types.PrimitiveInt)
	byteType := types.Builtin(types.PrimitiveUint8)
	code := ir.MicroCode{
		Types: []types.TypeRef{byteType, integer},
		Operands: []ir.RegisterFlow{
			{Inputs: []ir.Operand{{Kind: ir.OperandConstant, Index: 0}, {Kind: ir.OperandConstant, Index: 1}}, Outputs: []uint32{0}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}, Outputs: []uint32{1}},
		},
	}
	convert, _ := code.Descriptors.Append(ir.TypePayload{Type: integer})
	code.Instructions = []ir.MicroInstruction{{Op: ir.OpLoadIndex, Operands: 0}, {Op: ir.OpConvert, Descriptor: convert, Operands: 1}}
	constants := []ir.Constant{{Type: stringType}, {Type: integer}}
	if err := fuseIndexConvert(&code, &hir.Function{}, &types.TypeTable{}, constants, nil); err != nil {
		t.Fatal(err)
	}
	if code.Instructions[0].Op != ir.OpIndexConvert {
		t.Fatal("did not fuse a typed constant string/index pair")
	}
}

func TestDirectLocalResultsFusesMapIteratorOutputs(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	boolean := types.Builtin(types.PrimitiveBool)
	code := ir.MicroCode{
		Types: []types.TypeRef{integer, integer, boolean},
		Operands: []ir.RegisterFlow{
			{Outputs: []uint32{0, 1, 2}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 0}}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 1}}},
			{Inputs: []ir.Operand{{Kind: ir.OperandSlot, Index: 2}}},
		},
	}
	mapNext, err := code.Descriptors.Append(ir.LocalPayload{Local: "iter"})
	if err != nil {
		t.Fatal(err)
	}
	storeKey, _ := code.Descriptors.Append(ir.LocalPayload{Local: "key"})
	storeValue, _ := code.Descriptors.Append(ir.LocalPayload{Local: "value"})
	storeOK, _ := code.Descriptors.Append(ir.LocalPayload{Local: "ok"})
	code.Instructions = []ir.MicroInstruction{
		{Op: ir.OpMapIterNext, Descriptor: mapNext, Operands: 0},
		{Op: ir.OpStoreLocal, Descriptor: storeKey, Operands: 1},
		{Op: ir.OpStoreLocal, Descriptor: storeValue, Operands: 2},
		{Op: ir.OpStoreLocal, Descriptor: storeOK, Operands: 3},
	}
	function := hir.Function{Locals: []hir.Local{
		{ID: "iter", Type: integer},
		{ID: "key", Type: integer},
		{ID: "value", Type: integer},
		{ID: "ok", Type: boolean},
	}}
	if err := directLocalResults(&code, &function, &types.TypeTable{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(code.Instructions) != 1 {
		t.Fatalf("expected one map iterator instruction, got %#v", code.Instructions)
	}
	outputs := code.Operands[code.Instructions[0].Operands].Outputs
	if len(outputs) != 3 || outputs[0] != ir.LocalOutput|1 || outputs[1] != ir.LocalOutput|2 || outputs[2] != ir.LocalOutput|3 {
		t.Fatalf("map iterator outputs were not routed to locals: %v", outputs)
	}
}
