package bytecode

import (
	"encoding/json"
	"errors"

	"github.com/d7z-team/mini-go/compiler/types"
)

// OperandKind selects immutable constants or private frame temporaries. Named
// locals retain their separate, addressable identity across temporary reuse.
type OperandKind uint8

const (
	OperandSlot OperandKind = iota
	OperandConstant
	// OperandLocal reads a private scalar/pointer, or borrows a private
	// aggregate for an immediate scalar/pointer field-path read.
	OperandLocal
)

// IsDirectLocalType reports whether a local's value can be read without an
// aggregate copy. The producer must separately prove that the local cell is
// neither addressed nor captured; a pointer's target keeps its usual ownership.
func IsDirectLocalType(table *types.TypeTable, typ types.TypeRef) bool {
	underlying := types.View(table, typ).Underlying()
	return underlying.Kind == types.Pointer || underlying.Kind == types.Primitive &&
		underlying.Primitive >= types.PrimitiveBool && underlying.Primitive <= types.PrimitiveComplex128
}

type Operand struct {
	Kind  OperandKind `json:"kind"`
	Index uint32      `json:"index"`
}

// SlotInstruction has a fixed representation. Variable operands and operation
// metadata belong to the function's immutable descriptor table.
type SlotInstruction struct {
	Op         Opcode `json:"op"`
	Descriptor uint32 `json:"descriptor"`
	Operands   uint32 `json:"operands"`
}

func (instruction SlotInstruction) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]uint32{uint32(instruction.Op), instruction.Descriptor, instruction.Operands})
}

func (instruction *SlotInstruction) UnmarshalJSON(data []byte) error {
	var fields []uint32
	if err := strictUnmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 3 || fields[0] > 65535 {
		return errors.New("instruction requires [opcode:u16, descriptor:u32, operands:u32]")
	}
	*instruction = SlotInstruction{Op: Opcode(fields[0]), Descriptor: fields[1], Operands: fields[2]}
	return nil
}

func (operand Operand) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]uint32{uint32(operand.Kind), operand.Index})
}

func (operand *Operand) UnmarshalJSON(data []byte) error {
	var fields []uint32
	if err := strictUnmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 2 || fields[0] > 255 {
		return errors.New("operand requires [kind:u8, index:u32]")
	}
	*operand = Operand{Kind: OperandKind(fields[0]), Index: fields[1]}
	return nil
}

type SlotOperands struct {
	Inputs []Operand `json:"inputs,omitempty"`
	// Outputs use bit 31 for a private local destination, otherwise a temporary.
	// Direct local results are restricted to fixed-size scalar producers.
	Outputs       []uint32 `json:"outputs,omitempty"`
	Release       []uint32 `json:"release,omitempty"`
	ReleaseBefore []uint32 `json:"release_before,omitempty"`
}

const LocalOutput uint32 = 1 << 31

func IsDirectOutputType(table *types.TypeTable, typ types.TypeRef) bool {
	underlying := types.View(table, typ).Underlying()
	return underlying.Kind == types.Primitive && (underlying.Primitive == types.PrimitiveBool ||
		underlying.Primitive >= types.PrimitiveInt && underlying.Primitive <= types.PrimitiveComplex128)
}

func HasDirectOutput(op Opcode) bool {
	return op == OpUnary || op == OpBinary || op == OpZero || op == OpLen || op == OpCap || op == OpGetPath || op == OpConvert || op == OpLoadIndex
}

type SlotCode struct {
	Types        []types.TypeRef   `json:"types,omitempty"`
	Instructions []SlotInstruction `json:"instructions,omitempty"`
	Operands     []SlotOperands    `json:"operands,omitempty"`
	Descriptors  DescriptorTables  `json:"descriptors"`
}

// Operations expands descriptor references for linkage and preparation. It
// does not infer operand flow; that is encoded in Code.Operands.
func (fn *Function) Operations() ([]Instruction, error) {
	if fn.Code == nil {
		return nil, errors.New("missing slot code")
	}
	operations := make([]Instruction, len(fn.Code.Instructions))
	for i, instruction := range fn.Code.Instructions {
		if !IsKnownOpcode(instruction.Op) {
			return nil, newCodedValidationError(ValidationOpcodeUnknown, "instructions", errors.New("unknown opcode"))
		}
		payload, err := fn.Code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		if err != nil {
			return nil, err
		}
		operations[i] = Instruction{Op: instruction.Op, Payload: payload}
	}
	return operations, nil
}
