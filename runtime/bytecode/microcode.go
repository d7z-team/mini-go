package bytecode

import (
	"encoding/json"
	"errors"

	"github.com/d7z-team/mini-go/compiler/types"
)

// OperandKind selects immutable constants, private frame locals, or private
// frame temporaries. Named locals retain their separate, addressable identity
// across temporary reuse; an OperandLocal is only valid for a local that has
// not escaped through an address or capture.
type OperandKind uint8

const (
	OperandSlot OperandKind = iota
	OperandConstant
	// OperandLocal reads a private local directly. Aggregate values are read
	// through the instruction's value boundary, so the destination operation
	// remains responsible for any required value copy.
	OperandLocal
)

// IsDirectLocalType reports whether a local's scalar or pointer lane can be
// read without materializing a general aggregate value. The producer must
// separately prove that the local cell is neither addressed nor captured.
func IsDirectLocalType(table *types.TypeTable, typ types.TypeRef) bool {
	underlying := types.View(table, typ).Underlying()
	return underlying.Kind == types.Pointer || underlying.Kind == types.Primitive &&
		underlying.Primitive >= types.PrimitiveBool && underlying.Primitive <= types.PrimitiveComplex128
}

// IsDirectLocalValueType reports whether a valid local can be used as an
// immediate value operand or destination. Aggregate locals are included: the
// instruction boundary performs the language-required value copy when it
// publishes a result. Escape analysis is intentionally separate from this
// shape check and is enforced by bytecode validation.
func IsDirectLocalValueType(table *types.TypeTable, typ types.TypeRef) bool {
	return typ.Valid()
}

type Operand struct {
	Kind  OperandKind `json:"kind"`
	Index uint32      `json:"index"`
}

// MicroInstruction is the fixed 16-byte micro-op carried by the execution
// image. Mode contains the statically selected primitive lane. Descriptor and
// Operands index immutable cold tables; Cost is the logical fuel charged by
// this dispatch and may exceed one for a fused operation.
type MicroInstruction struct {
	Op         Opcode `json:"op"`
	Mode       uint16 `json:"mode"`
	Descriptor uint32 `json:"descriptor"`
	Operands   uint32 `json:"operands"`
	Cost       uint32 `json:"cost"`
}

func (instruction MicroInstruction) MarshalJSON() ([]byte, error) {
	word := uint32(instruction.Op) | uint32(instruction.Mode)<<16
	return json.Marshal([4]uint32{word, instruction.Descriptor, instruction.Operands, instruction.LogicalCost()})
}

func (instruction *MicroInstruction) UnmarshalJSON(data []byte) error {
	var fields []uint32
	if err := strictUnmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 4 || fields[3] == 0 {
		return errors.New("instruction requires [opcode:u16|mode:u16, descriptor:u32, operands:u32, cost:u32]")
	}
	*instruction = MicroInstruction{Op: Opcode(fields[0] & 0xffff), Mode: uint16(fields[0] >> 16), Descriptor: fields[1], Operands: fields[2], Cost: fields[3]}
	return nil
}

func (instruction MicroInstruction) LogicalCost() uint32 {
	if instruction.Cost == 0 {
		return 1
	}
	return instruction.Cost
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

type RegisterFlow struct {
	Inputs []Operand `json:"inputs,omitempty"`
	// Outputs use bit 31 for a private local destination, otherwise a temporary.
	// Direct local results are written into private local storage. Aggregate
	// results are allowed when the producer has a complete value boundary.
	Outputs       []uint32 `json:"outputs,omitempty"`
	Release       []uint32 `json:"release,omitempty"`
	ReleaseBefore []uint32 `json:"release_before,omitempty"`
}

const LocalOutput uint32 = 1 << 31

func IsDirectOutputType(table *types.TypeTable, typ types.TypeRef) bool {
	return IsDirectLocalValueType(table, typ)
}

func HasDirectOutput(op Opcode) bool {
	return op == OpUnary || op == OpBinary || op == OpMinMax || op == OpZero || op == OpLen || op == OpCap || op == OpGetPath || op == OpConvert || op == OpLoadIndex || op == OpStringNextRuneIndex || op == OpIndexConvert || op == OpCallDirect || op == OpMapIterNext
}

// AllowsDirectLocalOutputs reports whether an instruction can publish one or
// more complete results directly into private locals. Calls and map iteration
// have fixed multi-result boundaries; all other producers have one result.
func AllowsDirectLocalOutputs(op Opcode, count int) bool {
	if count == 0 {
		return false
	}
	if op == OpCallDirect || op == OpMapIterNext {
		return true
	}
	return count == 1 && HasDirectOutput(op)
}

type MicroCode struct {
	Types        []types.TypeRef    `json:"types,omitempty"`
	Instructions []MicroInstruction `json:"instructions,omitempty"`
	Blocks       []BasicBlock       `json:"blocks,omitempty"`
	Operands     []RegisterFlow     `json:"operands,omitempty"`
	Descriptors  DescriptorTables   `json:"descriptors"`
}

const NoBlock uint32 = ^uint32(0)

// BasicBlock describes a maximal straight-line executable range after labels
// have been removed. First and Count use executable PCs rather than wire PCs.
type BasicBlock struct {
	First       uint32 `json:"first"`
	Count       uint32 `json:"count"`
	Fallthrough uint32 `json:"fallthrough"`
	Cost        uint32 `json:"cost"`
}

// Operations expands descriptor references for linkage and preparation. It
// does not infer operand flow; that is encoded in Code.Operands.
func (fn *Function) Operations() ([]Instruction, error) {
	if fn.Code == nil {
		return nil, errors.New("missing microcode")
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
