package bytecode

import (
	"encoding/json"
	"fmt"
)

// Payload is a typed instruction descriptor. Only bytecode descriptor models
// implement it. Images decode descriptors at their input boundary.
type Payload interface {
	instructionPayload()
	descriptorBytes() int
}

// DescriptorBytes reports the logical storage of a descriptor, including its
// strings and variable-length tables, independently of the JSON representation.
func DescriptorBytes(payload Payload) int {
	if payload == nil {
		return 0
	}
	return descriptorBytes(payload)
}

func (inst Instruction) MarshalJSON() ([]byte, error) {
	w := canonicalBuffer{}
	w.encodeInstruction(&inst)
	return w.data, w.err
}

func (inst *Instruction) UnmarshalJSON(data []byte) error {
	var wire struct {
		Op      string          `json:"op"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	op, ok := ParseOpcode(wire.Op)
	if !ok {
		return newCodedValidationError(ValidationOpcodeUnknown, "instruction.op", fmt.Errorf("unknown opcode %q", wire.Op))
	}
	payload, err := decodePayload(wire.Op, wire.Payload)
	if err != nil {
		return err
	}
	*inst = Instruction{Op: op, Payload: payload}
	return nil
}
