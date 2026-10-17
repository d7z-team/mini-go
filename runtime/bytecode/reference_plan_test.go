package bytecode

import (
	"math/bits"
	"slices"
	"testing"
)

func TestFrameReferencesFollowHiddenLocalsAndTemporaryLifetime(t *testing.T) {
	fn := Function{
		Locals: []Local{{ID: "source", Type: testType("String")}, {ID: "destination", Type: testType("String")}, {ID: "scalar", Type: testType("Int")}},
		Code: testSlotCode([]string{"String", "String"}, []Instruction{
			{Op: OpLoadLocal, Payload: LocalPayload{Local: "source"}},
			{Op: OpStoreLocal, Payload: LocalPayload{Local: "destination"}},
			{Op: OpLoadLocal, Payload: LocalPayload{Local: "destination"}},
			{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}}),
	}
	plan, err := PrepareFrameReferences(&fn, []bool{true, true, false}, []bool{true, true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertReferenceRoots(t, plan, 0, 0)
	assertReferenceRoots(t, plan, 1, 3)
	assertReferenceRoots(t, plan, 2, 1)
	assertReferenceRoots(t, plan, 3, 4)
	assertReferenceRoots(t, plan, 4)
}

func TestFrameReferencesPreserveStorageIdentityBeforeAssignment(t *testing.T) {
	fn := Function{
		Locals: []Local{{ID: "destination", Type: testType("Array<32, Int>")}},
		Code: testSlotCode([]string{"Array<32, Int>"}, []Instruction{
			{Op: OpZero, Payload: TypePayload{Type: testType("Array<32, Int>")}},
			{Op: OpStoreLocal, Payload: LocalPayload{Local: "destination"}},
			{Op: OpReturn, Payload: ReturnPayload{}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}}),
	}
	for _, rebind := range []bool{false, true} {
		fn.Code.Descriptors.Local[fn.Code.Instructions[1].Descriptor] = LocalPayload{Local: "destination", Rebind: rebind}
		plan, err := PrepareFrameReferences(&fn, []bool{true}, []bool{true}, []bool{true})
		if err != nil {
			t.Fatal(err)
		}
		assertReferenceRoots(t, plan, 0, 0)
		assertReferenceRoots(t, plan, 1, 0, 1)
		assertReferenceRoots(t, plan, 2)
	}
}

func TestFrameReferencesTypeDispatchKillsOnlySelectedBinding(t *testing.T) {
	for _, preserveDefault := range []bool{false, true} {
		defaultValue := "fallback"
		if preserveDefault {
			defaultValue = "binding"
		}
		fn := Function{
			Locals: []Local{{ID: "subject", Type: testType("Any")}, {ID: "binding", Type: testType("String")}, {ID: "fallback", Type: testType("String")}},
			Code: testSlotCode([]string{"String"}, []Instruction{
				{Op: OpTypeDispatch, Payload: TypeDispatchPayload{Subject: "subject", Default: "default", Cases: []TypeCase{{Type: testType("String"), Label: "match", Binding: "binding"}}}},
				{Op: OpLabel, Payload: LabelPayload{Label: "match"}},
				{Op: OpLoadLocal, Payload: LocalPayload{Local: "binding"}},
				{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
				{Op: OpLabel, Payload: LabelPayload{Label: "default"}},
				{Op: OpLoadLocal, Payload: LocalPayload{Local: defaultValue}},
				{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
			}, [][2][]uint32{{nil, nil}, {nil, nil}, {nil, {0}}, {{0}, nil}, {nil, nil}, {nil, {0}}, {{0}, nil}}),
		}
		plan, err := PrepareFrameReferences(&fn, []bool{true, true, true}, []bool{true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if preserveDefault {
			assertReferenceRoots(t, plan, 0, 0, 1)
		} else {
			assertReferenceRoots(t, plan, 0, 0, 2)
		}
		assertReferenceRoots(t, plan, 1, 1)
		assertReferenceRoots(t, plan, 2, 1)
		assertReferenceRoots(t, plan, 3, 3)
	}
}

func TestFrameReferencesSelectKeepsUnselectedReceiveValue(t *testing.T) {
	fn := Function{
		Locals: []Local{
			{ID: "receive", Type: testType("Any")},
			{ID: "send", Type: testType("Any")},
			{ID: "message", Type: testType("String")},
			{ID: "value", Type: testType("String")},
			{ID: "ok", Type: testType("Bool")},
			{ID: "index", Type: testType("Int")},
		},
		Code: testSlotCode([]string{"String"}, []Instruction{
			{Op: OpSelect, Payload: SelectPayload{Index: "index", Default: true, Cases: []SelectCase{
				{Channel: "receive", Value: "value", OK: "ok"}, {Channel: "send", Send: "message"},
			}}},
			{Op: OpLoadLocal, Payload: LocalPayload{Local: "value"}},
			{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, nil}, {nil, {0}}, {{0}, nil}}),
	}
	plan, err := PrepareFrameReferences(&fn, []bool{true, true, true, true, false, false}, []bool{true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertReferenceRoots(t, plan, 0, 0, 1, 2, 3)
	assertReferenceRoots(t, plan, 1, 3)
	assertReferenceRoots(t, plan, 2, 6)
}

func TestFrameReferencesDistinguishRebindingAndWritingCapturedCell(t *testing.T) {
	fn := Function{
		Locals: []Local{{ID: "captured", Type: testType("Int")}},
		Code: testSlotCode([]string{"Function"}, []Instruction{
			{Op: OpStoreLocal, Payload: LocalPayload{Local: "captured", Rebind: true}},
			{Op: OpMakeClosure, Payload: ClosurePayload{Function: "closure", Captures: []AddressPayload{{Kind: "local", Local: "captured"}}}},
			{Op: OpStoreLocal, Payload: LocalPayload{Local: "captured"}},
			{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, nil}, {nil, {0}}, {nil, nil}, {{0}, nil}}),
	}
	// Constant operands do not hold per-frame guest storage.
	fn.Code.Operands[0].Inputs = []Operand{{Kind: OperandConstant, Index: 0}}
	fn.Code.Operands[2].Inputs = []Operand{{Kind: OperandConstant, Index: 1}}
	plan, err := PrepareFrameReferences(&fn, []bool{false}, []bool{true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Cells[0] {
		t.Fatal("captured scalar must use a stable cell")
	}
	assertReferenceRoots(t, plan, 0)
	assertReferenceRoots(t, plan, 1, 0)
	assertReferenceRoots(t, plan, 2, 0, 1)
	assertReferenceRoots(t, plan, 3, 1)
}

func TestFrameReferencesLoopAndNamedResultUnwind(t *testing.T) {
	fn := Function{
		Locals:       []Local{{ID: "result", Type: testType("String")}, {ID: "loop", Type: testType("String")}},
		ResultLocals: []string{"result"},
		Upvalues:     []Upvalue{{ID: "shared", Type: testType("Int")}},
		Code: testSlotCode([]string{"Bool", "String"}, []Instruction{
			{Op: OpLabel, Payload: LabelPayload{Label: "again"}},
			{Op: OpLoadLocal, Payload: LocalPayload{Local: "loop"}},
			{Op: OpStoreUpvalue, Payload: UpvaluePayload{Upvalue: "shared"}},
			{Op: OpJumpIf, Payload: JumpPayload{Label: "again"}},
			{Op: OpPanic},
		}, [][2][]uint32{{nil, nil}, {nil, {1}}, {nil, nil}, {{0}, nil}, {{1}, nil}}),
	}
	fn.Code.Operands[2].Inputs = []Operand{{Kind: OperandConstant, Index: 0}}
	plan, err := PrepareFrameReferences(&fn, []bool{true, true}, []bool{false, true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertReferenceRoots(t, plan, 0, 0, 1, 4)
	assertReferenceRoots(t, plan, 1, 0, 1, 4)
	assertReferenceRoots(t, plan, 2, 0, 1, 3, 4)
	assertReferenceRoots(t, plan, 3, 0, 1, 3, 4)
	assertReferenceRoots(t, plan, 4, 0, 3)
	assertReferenceRoots(t, plan, 5, 0)
}

func TestFrameReferencesValidateReleaseAcrossInstructionBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		pc     int
		before bool
		valid  bool
	}{
		{name: "dead before definition", pc: 0, before: true, valid: true},
		{name: "before consuming", pc: 1, before: true},
		{name: "before future use", pc: 0},
		{name: "after last use", pc: 1, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := Function{Code: testSlotCode([]string{"String"}, []Instruction{
				{Op: OpZero, Payload: TypePayload{Type: testType("String")}},
				{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
			}, [][2][]uint32{{nil, {0}}, {{0}, nil}})}
			if test.before {
				fn.Code.Operands[test.pc].ReleaseBefore = []uint32{0}
			} else {
				fn.Code.Operands[test.pc].Release = []uint32{0}
			}
			_, err := PrepareFrameReferences(&fn, nil, []bool{true}, nil)
			if (err == nil) != test.valid {
				t.Fatalf("reference plan error = %v, valid = %t", err, test.valid)
			}
		})
	}
}

func assertReferenceRoots(t *testing.T, plan FrameReferences, pc int, want ...int) {
	t.Helper()
	var got []int
	for word, bitmap := range plan.Maps[plan.AtPC[pc]] {
		for bitmap != 0 {
			got = append(got, word*64+bits.TrailingZeros64(bitmap))
			bitmap &= bitmap - 1
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("PC %d roots = %v, want %v", pc, got, want)
	}
}
