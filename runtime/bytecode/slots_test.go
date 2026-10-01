package bytecode

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func BenchmarkSlotInitializationLinear(b *testing.B) {
	for _, size := range []int{128, 4096} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			integer := types.Builtin(types.PrimitiveInt)
			code := SlotCode{Types: []types.TypeRef{integer}, Operands: []SlotOperands{{Outputs: []uint32{0}}, {Inputs: []Operand{{Kind: OperandSlot}}, Release: []uint32{0}}}}
			zero, _ := code.Descriptors.Append(TypePayload{Type: integer})
			stored, _ := code.Descriptors.Append(LocalPayload{Local: "value"})
			for range size {
				code.Instructions = append(code.Instructions, SlotInstruction{Op: OpZero, Descriptor: zero}, SlotInstruction{Op: OpStoreLocal, Descriptor: stored, Operands: 1})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := ValidateSlotCode(&code, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestSlotCodeRoundTripPreservesNumericOperationsAndConcreteDescriptors(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	code := SlotCode{Types: []types.TypeRef{integer}, Operands: []SlotOperands{
		{Outputs: []uint32{0}}, {Inputs: []Operand{{Kind: OperandSlot, Index: 0}}, Release: []uint32{0}},
	}}
	zero, err := code.Descriptors.Append(TypePayload{Type: integer})
	if err != nil {
		t.Fatal(err)
	}
	returned, err := code.Descriptors.Append(ReturnPayload{ResultCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []SlotInstruction{{Op: OpZero, Descriptor: zero, Operands: 0}, {Op: OpReturn, Descriptor: returned, Operands: 1}}
	if err := ValidateSlotCode(&code, nil); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeCanonicalValue(code)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SlotCode
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSlotCode(&decoded, nil); err != nil {
		t.Fatal(err)
	}
	reencoded, err := encodeCanonicalValue(decoded)
	if err != nil || string(encoded) != string(reencoded) {
		t.Fatalf("unstable slot encoding: %s / %s / %v", encoded, reencoded, err)
	}
	var shape struct {
		Instructions [][3]uint32 `json:"instructions"`
	}
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatal(err)
	}
	if Opcode(shape.Instructions[0][0]) != OpZero {
		t.Fatalf("unexpected encoded opcode: %s", encoded)
	}
}

func TestSlotInitializationRejectsMissingBranchDefinitionAndUseAfterRelease(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	boolean := types.Builtin(types.PrimitiveBool)
	for _, release := range []bool{false, true} {
		code := SlotCode{Types: []types.TypeRef{integer}}
		appendInstruction := func(op Opcode, payload Payload, operands SlotOperands) {
			index, err := code.Descriptors.Append(payload)
			if err != nil {
				t.Fatal(err)
			}
			code.Instructions = append(code.Instructions, SlotInstruction{Op: op, Descriptor: index, Operands: uint32(len(code.Operands))})
			code.Operands = append(code.Operands, operands)
		}
		if !release {
			appendInstruction(OpJumpIf, JumpPayload{Label: "join"}, SlotOperands{Inputs: []Operand{{Kind: OperandConstant, Index: 0}}})
		}
		initialization := SlotOperands{Outputs: []uint32{0}}
		if release {
			initialization.Release = []uint32{0}
		}
		appendInstruction(OpZero, TypePayload{Type: integer}, initialization)
		appendInstruction(OpLabel, LabelPayload{Label: "join"}, SlotOperands{})
		appendInstruction(OpReturn, ReturnPayload{ResultCount: 1}, SlotOperands{Inputs: []Operand{{Kind: OperandSlot, Index: 0}}})
		err := ValidateSlotCode(&code, []Constant{{ID: "branch", Type: boolean, Value: json.RawMessage("true")}})
		if err == nil || !strings.Contains(err.Error(), "uninitialized") {
			t.Fatalf("release=%v: expected uninitialized operand, got %v", release, err)
		}
	}
}

func TestSlotInitializationRechecksBackedgesAndEntryReleases(t *testing.T) {
	integer, boolean := types.Builtin(types.PrimitiveInt), types.Builtin(types.PrimitiveBool)
	for _, test := range []struct {
		name                       string
		release, redefine, onEntry bool
		valid                      bool
	}{
		{name: "retained", valid: true},
		{name: "released_backedge", release: true},
		{name: "redefined_backedge", release: true, redefine: true, valid: true},
		{name: "entry_releases_input", onEntry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			code := SlotCode{Types: []types.TypeRef{integer}}
			appendInstruction := func(op Opcode, payload Payload, operands SlotOperands) {
				descriptor, err := code.Descriptors.Append(payload)
				if err != nil {
					t.Fatal(err)
				}
				code.Instructions = append(code.Instructions, SlotInstruction{Op: op, Descriptor: descriptor, Operands: uint32(len(code.Operands))})
				code.Operands = append(code.Operands, operands)
			}
			appendInstruction(OpZero, TypePayload{Type: integer}, SlotOperands{Outputs: []uint32{0}})
			appendInstruction(OpLabel, LabelPayload{Label: "again"}, SlotOperands{})
			if test.redefine {
				appendInstruction(OpZero, TypePayload{Type: integer}, SlotOperands{Outputs: []uint32{0}})
			}
			read := SlotOperands{Inputs: []Operand{{Kind: OperandSlot}}}
			if test.release {
				read.Release = []uint32{0}
			}
			if test.onEntry {
				read.ReleaseBefore = []uint32{0}
			}
			appendInstruction(OpStoreLocal, LocalPayload{Local: "value"}, read)
			appendInstruction(OpJumpIf, JumpPayload{Label: "again"}, SlotOperands{Inputs: []Operand{{Kind: OperandConstant}}})
			appendInstruction(OpZero, TypePayload{Type: integer}, SlotOperands{Outputs: []uint32{0}})
			appendInstruction(OpReturn, ReturnPayload{ResultCount: 1}, SlotOperands{Inputs: []Operand{{Kind: OperandSlot}}, Release: []uint32{0}})
			// This read is unreachable, but still has a valid operand shape.
			appendInstruction(OpReturn, ReturnPayload{ResultCount: 1}, SlotOperands{Inputs: []Operand{{Kind: OperandSlot}}})
			err := ValidateSlotCode(&code, []Constant{{ID: "branch", Type: boolean, Value: json.RawMessage("true")}})
			if test.valid && err != nil || !test.valid && (err == nil || !strings.Contains(err.Error(), "uninitialized")) {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
}

func TestSlotComparisonDescriptorSurvivesCanonicalEncoding(t *testing.T) {
	code := SlotCode{}
	expected := CompareBranchPayload{Operator: "<=", Type: types.Builtin(types.PrimitiveInt), Label: "next", When: false}
	index, err := code.Descriptors.Append(expected)
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []SlotInstruction{{Op: OpCompareBranch, Descriptor: index}}
	encoded, err := encodeCanonicalValue(code)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SlotCode
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	actual, err := decoded.Descriptors.Payload(decoded.Instructions[0].Op, decoded.Instructions[0].Descriptor)
	if err != nil || actual != expected {
		t.Fatalf("comparison descriptor changed: %v / %v", actual, err)
	}
}

func TestSlotTypesRejectMismatchedStorageAndReturn(t *testing.T) {
	integer, boolean := types.Builtin(types.PrimitiveInt), types.Builtin(types.PrimitiveBool)
	for _, wrongReturn := range []bool{false, true} {
		artifact := NewArtifact("test", "test")
		code := SlotCode{Types: []types.TypeRef{boolean}, Operands: []SlotOperands{{Outputs: []uint32{0}}, {Inputs: []Operand{{Kind: OperandSlot, Index: 0}}}}}
		zeroType, returnType := integer, boolean
		if wrongReturn {
			zeroType, returnType = boolean, integer
		}
		zero, _ := code.Descriptors.Append(TypePayload{Type: zeroType})
		returned, _ := code.Descriptors.Append(ReturnPayload{ResultCount: 1})
		code.Instructions = []SlotInstruction{{Op: OpZero, Descriptor: zero, Operands: 0}, {Op: OpReturn, Descriptor: returned, Operands: 1}}
		artifact.Functions = []Function{{ID: "main", Signature: types.FunctionSignature{Results: []types.TypeRef{returnType}}, Code: &code}}
		if err := ValidateArtifact(&artifact); err == nil {
			t.Fatalf("wrongReturn=%v: accepted mismatched slot type", wrongReturn)
		}
	}
}

func TestDirectLocalOperandsRequirePrivateValueStorage(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	for _, kind := range []string{"scalar", "pointer"} {
		for _, access := range []string{"private", "address", "capture"} {
			t.Run(kind+"/"+access, func(t *testing.T) {
				artifact := NewArtifact("locals", "locals")
				pointer := types.TypeRef{Kind: types.Pointer, Node: "pointer"}
				artifact.TypeTable.Nodes = []types.TypeNode{{ID: "pointer", Kind: types.Pointer, Elem: integer}}
				valueType := integer
				if kind == "pointer" {
					valueType = pointer
					pointer = types.TypeRef{Kind: types.Pointer, Node: "pointerPointer"}
					artifact.TypeTable.Nodes = append(artifact.TypeTable.Nodes, types.TypeNode{ID: "pointerPointer", Kind: types.Pointer, Elem: valueType})
				}
				code := SlotCode{Types: []types.TypeRef{pointer}, Operands: []SlotOperands{{Inputs: []Operand{{Kind: OperandLocal}}}}}
				returned, err := code.Descriptors.Append(ReturnPayload{ResultCount: 1})
				if err != nil {
					t.Fatal(err)
				}
				code.Instructions = []SlotInstruction{{Op: OpReturn, Descriptor: returned}}
				artifact.Functions = []Function{{ID: "main", Signature: types.FunctionSignature{Results: []types.TypeRef{valueType}}, Locals: []Local{{ID: "value", Type: valueType}}, Code: &code}}
				// Even an unreachable address/capture invalidates the private-storage
				// proof; it must not depend on the caller choosing a particular path.
				if access != "private" {
					op := OpAddressOf
					var payload Payload = AddressPayload{Kind: "local", Local: "value"}
					if access == "capture" {
						op = OpMakeClosure
						payload = ClosurePayload{Function: "child", Captures: []AddressPayload{{Kind: "local", Local: "value"}}}
						artifact.Functions = append(artifact.Functions, Function{ID: "child", Upvalues: []Upvalue{{ID: "value", Type: valueType}}, Code: &SlotCode{}})
					}
					index, err := code.Descriptors.Append(payload)
					if err != nil {
						t.Fatal(err)
					}
					code.Instructions = append(code.Instructions, SlotInstruction{Op: op, Descriptor: index, Operands: 1})
					code.Operands = append(code.Operands, SlotOperands{Outputs: []uint32{0}})
				}
				err = ValidateArtifact(&artifact)
				if access == "private" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "private scalar") {
					t.Fatalf("shared local accepted as direct input: %v", err)
				}
			})
		}
	}
}

func TestSlotScalarTypesMatchOperatorResults(t *testing.T) {
	for _, test := range []struct {
		name, operator      string
		left, right, result types.PrimitiveKind
		invalid             bool
	}{
		{"integer addition", "+", types.PrimitiveInt, types.PrimitiveInt, types.PrimitiveInt, false},
		{"forged boolean destination", "+", types.PrimitiveInt, types.PrimitiveInt, types.PrimitiveBool, true},
		{"different integer widths", "+", types.PrimitiveInt, types.PrimitiveUint, types.PrimitiveInt, true},
		{"unsigned shift count", "<<", types.PrimitiveInt, types.PrimitiveUint, types.PrimitiveInt, false},
		{"comparison result", "<", types.PrimitiveInt, types.PrimitiveInt, types.PrimitiveBool, false},
		{"forged comparison destination", "<", types.PrimitiveInt, types.PrimitiveInt, types.PrimitiveInt, true},
		{"floating bitwise", "&", types.PrimitiveFloat64, types.PrimitiveFloat64, types.PrimitiveFloat64, true},
		{"complex construction", "complex", types.PrimitiveFloat32, types.PrimitiveFloat32, types.PrimitiveComplex64, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := NewArtifact("operators", "operators")
			artifact.Constants = []Constant{{ID: "left", Type: types.Builtin(test.left), Value: json.RawMessage(`1`)}, {ID: "right", Type: types.Builtin(test.right), Value: json.RawMessage(`2`)}}
			result := types.Builtin(test.result)
			code := SlotCode{Types: []types.TypeRef{result}, Operands: []SlotOperands{
				{Inputs: []Operand{{Kind: OperandConstant}, {Kind: OperandConstant, Index: 1}}, Outputs: []uint32{0}},
				{Inputs: []Operand{{Kind: OperandSlot}}, Release: []uint32{0}},
			}}
			operation, _ := code.Descriptors.Append(OperatorPayload{Operator: test.operator})
			returned, _ := code.Descriptors.Append(ReturnPayload{ResultCount: 1})
			code.Instructions = []SlotInstruction{{Op: OpBinary, Descriptor: operation}, {Op: OpReturn, Descriptor: returned, Operands: 1}}
			artifact.Functions = []Function{{ID: "main", Signature: types.FunctionSignature{Results: []types.TypeRef{result}}, Code: &code}}
			if err := ValidateArtifact(&artifact); (err != nil) != test.invalid {
				t.Fatalf("invalid=%v: %v", test.invalid, err)
			}
		})
	}
}

func TestSlotOperandsRejectDuplicateDestinationsAndLabelEffects(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	for _, label := range []bool{false, true} {
		code := SlotCode{Types: []types.TypeRef{integer, types.Builtin(types.PrimitiveBool)}}
		var op Opcode
		var payload Payload
		var operands SlotOperands
		if label {
			op, payload, operands = OpLabel, LabelPayload{Label: "entry"}, SlotOperands{Release: []uint32{0}}
		} else {
			op, payload = OpTypeAssertOK, TypePayload{Type: integer}
			operands = SlotOperands{Inputs: []Operand{{Kind: OperandConstant}}, Outputs: []uint32{0, 0}}
		}
		index, _ := code.Descriptors.Append(payload)
		code.Instructions, code.Operands = []SlotInstruction{{Op: op, Descriptor: index}}, []SlotOperands{operands}
		if err := ValidateSlotCode(&code, []Constant{{Type: integer}}); err == nil {
			t.Fatalf("label=%v: accepted ambiguous operands", label)
		}
	}
}

func TestSlotTupleDecodingRejectsTruncationOverflowAndExtraValuesAtomically(t *testing.T) {
	for _, raw := range []string{`[]`, `[1,2]`, `[1,2,3,4]`, `[65536,0,0]`, `[1,-1,0]`, `[1,0,4294967296]`, `[1,0,0] true`} {
		instruction := SlotInstruction{Op: OpReturn, Descriptor: 7, Operands: 8}
		before := instruction
		if err := json.Unmarshal([]byte(raw), &instruction); err == nil || instruction != before {
			t.Fatalf("instruction %s changed on failure: %+v / %v", raw, instruction, err)
		}
	}
	for _, raw := range []string{`[]`, `[1]`, `[1,2,3]`, `[256,0]`, `[1,-1]`, `[1,4294967296]`} {
		operand := Operand{Kind: OperandConstant, Index: 9}
		before := operand
		if err := json.Unmarshal([]byte(raw), &operand); err == nil || operand != before {
			t.Fatalf("operand %s changed on failure: %+v / %v", raw, operand, err)
		}
	}
}

func TestSlotCollectionAccessChecksKeysValuesAndSizes(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	boolean := types.Builtin(types.PrimitiveBool)
	text := types.Builtin(types.PrimitiveString)
	sequence := types.TypeRef{Kind: types.Slice, Node: "sequence"}
	mapping := types.TypeRef{Kind: types.Map, Node: "mapping"}
	record := types.TypeRef{Kind: types.Struct, Node: "record"}
	channel := types.TypeRef{Kind: types.Waitable, Node: "channel"}
	for _, test := range []struct {
		name         string
		op           Opcode
		payload      Payload
		inputs       []types.TypeRef
		outputs      []types.TypeRef
		invalidInput int
	}{
		{"slice index", OpLoadIndex, nil, []types.TypeRef{sequence, integer}, []types.TypeRef{integer}, 1},
		{"slice write", OpStoreIndex, nil, []types.TypeRef{sequence, integer, integer}, nil, 2},
		{"map key", OpLoadIndexOK, nil, []types.TypeRef{mapping, text}, []types.TypeRef{integer, boolean}, 1},
		{"map write", OpStoreIndex, nil, []types.TypeRef{mapping, text, integer}, nil, 2},
		{"map constructor key", OpMakeMap, MakeMapPayload{Type: mapping, EntryCount: 1}, []types.TypeRef{text, integer}, []types.TypeRef{mapping}, 0},
		{"map constructor value", OpMakeMap, MakeMapPayload{Type: mapping, EntryCount: 1}, []types.TypeRef{text, integer}, []types.TypeRef{mapping}, 1},
		{"map capacity", OpMakeMap, MakeMapPayload{Type: mapping, HasCapacity: true}, []types.TypeRef{integer}, []types.TypeRef{mapping}, 0},
		{"slice size", OpMakeSlice, MakeSlicePayload{Type: sequence}, []types.TypeRef{integer}, []types.TypeRef{sequence}, 0},
		{"field write", OpStoreField, FieldPayload{Field: "value"}, []types.TypeRef{record, integer}, nil, 1},
		{"channel send", OpWaitableSend, nil, []types.TypeRef{channel, integer}, nil, 1},
		{"channel try send", OpWaitableTrySend, nil, []types.TypeRef{channel, integer}, []types.TypeRef{boolean}, 1},
		{"channel receive", OpWaitableRecvOK, nil, []types.TypeRef{channel}, []types.TypeRef{integer, boolean}, 0},
		{"channel try receive", OpWaitableTryRecv, nil, []types.TypeRef{channel}, []types.TypeRef{integer, boolean}, 0},
		{"channel readiness", OpWaitableCanSend, nil, []types.TypeRef{channel}, []types.TypeRef{boolean}, 0},
		{"channel close", OpWaitableClose, nil, []types.TypeRef{channel}, nil, 0},
		{"rune input", OpStringRuneAt, nil, []types.TypeRef{text, integer}, []types.TypeRef{types.Builtin(types.PrimitiveInt32)}, 0},
		{"rune index", OpStringNextRuneIndex, nil, []types.TypeRef{text, integer}, []types.TypeRef{integer}, 1},
		{"map delete", OpDelete, nil, []types.TypeRef{mapping, text}, nil, 1},
		{"slice clear", OpClear, nil, []types.TypeRef{sequence}, nil, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, valid := range []bool{true, false} {
				artifact := NewArtifact("collections", "collections")
				artifact.TypeTable.Nodes = []types.TypeNode{
					{ID: "sequence", Kind: types.Slice, Elem: integer},
					{ID: "mapping", Kind: types.Map, Key: text, Elem: integer},
					{ID: "record", Kind: types.Struct, Fields: []types.Field{{Name: "value", Type: integer}}},
					{ID: "channel", Kind: types.Waitable, Direction: types.ChannelBoth, Elem: integer},
				}
				code := SlotCode{Types: append([]types.TypeRef(nil), test.inputs...)}
				if !valid {
					code.Types[test.invalidInput] = boolean
				}
				appendOperation := func(op Opcode, payload Payload, operands SlotOperands) {
					descriptor, err := code.Descriptors.Append(payload)
					if err != nil {
						t.Fatal(err)
					}
					code.Instructions = append(code.Instructions, SlotInstruction{Op: op, Descriptor: descriptor, Operands: uint32(len(code.Operands))})
					code.Operands = append(code.Operands, operands)
				}
				var operands SlotOperands
				for i, typ := range code.Types {
					appendOperation(OpZero, TypePayload{Type: typ}, SlotOperands{Outputs: []uint32{uint32(i)}})
					operands.Inputs = append(operands.Inputs, Operand{Kind: OperandSlot, Index: uint32(i)})
				}
				for _, typ := range test.outputs {
					operands.Outputs = append(operands.Outputs, uint32(len(code.Types)))
					code.Types = append(code.Types, typ)
				}
				appendOperation(test.op, test.payload, operands)
				appendOperation(OpReturn, ReturnPayload{}, SlotOperands{})
				artifact.Functions = []Function{{ID: "main", Code: &code}}
				if err := ValidateArtifact(&artifact); (err == nil) != valid {
					t.Fatalf("valid=%v: %v", valid, err)
				}
			}
		})
	}
}

func TestSlotChannelAccessHonorsDirectionAndResultTypes(t *testing.T) {
	integer, boolean := types.Builtin(types.PrimitiveInt), types.Builtin(types.PrimitiveBool)
	channel := types.TypeRef{Kind: types.Waitable, Node: "channel"}
	for _, direction := range []types.ChannelDir{types.ChannelBoth, types.ChannelSend, types.ChannelReceive} {
		for _, operation := range []Opcode{OpWaitableSend, OpWaitableTrySend, OpWaitableRecv, OpWaitableRecvOK, OpWaitableTryRecv, OpWaitableCanSend, OpWaitableCanRecv, OpWaitableClose} {
			for _, forgedResult := range []bool{false, true} {
				artifact := NewArtifact("channels", "channels")
				artifact.TypeTable.Nodes = []types.TypeNode{{ID: "channel", Kind: types.Waitable, Direction: direction, Elem: integer}}
				code := SlotCode{Types: []types.TypeRef{channel, integer}}
				channelZero, _ := code.Descriptors.Append(TypePayload{Type: channel})
				valueZero, _ := code.Descriptors.Append(TypePayload{Type: integer})
				returned, _ := code.Descriptors.Append(ReturnPayload{})
				code.Instructions = []SlotInstruction{{Op: OpZero, Descriptor: channelZero}, {Op: OpZero, Descriptor: valueZero, Operands: 1}, {Op: operation, Operands: 2}, {Op: OpReturn, Descriptor: returned, Operands: 3}}
				operands := SlotOperands{Inputs: []Operand{{Kind: OperandSlot, Index: 0}}}
				sending := operation == OpWaitableSend || operation == OpWaitableTrySend || operation == OpWaitableCanSend || operation == OpWaitableClose
				if operation == OpWaitableSend || operation == OpWaitableTrySend {
					operands.Inputs = append(operands.Inputs, Operand{Kind: OperandSlot, Index: 1})
				}
				if operation == OpWaitableRecv || operation == OpWaitableRecvOK || operation == OpWaitableTryRecv {
					operands.Outputs = append(operands.Outputs, uint32(len(code.Types)))
					code.Types = append(code.Types, integer)
				}
				if operation == OpWaitableRecvOK || operation == OpWaitableTryRecv || operation == OpWaitableTrySend || operation == OpWaitableCanSend || operation == OpWaitableCanRecv {
					operands.Outputs = append(operands.Outputs, uint32(len(code.Types)))
					code.Types = append(code.Types, boolean)
				}
				if forgedResult {
					if len(operands.Outputs) == 0 {
						continue
					}
					code.Types[operands.Outputs[0]] = types.Builtin(types.PrimitiveString)
				}
				code.Operands = []SlotOperands{{Outputs: []uint32{0}}, {Outputs: []uint32{1}}, operands, {}}
				artifact.Functions = []Function{{ID: "main", Code: &code}}
				valid := !forgedResult && (direction == types.ChannelBoth || sending == (direction == types.ChannelSend))
				if err := ValidateArtifact(&artifact); (err == nil) != valid {
					t.Fatalf("%s direction=%d forged=%v: %v", operation, direction, forgedResult, err)
				}
			}
		}
	}
}
