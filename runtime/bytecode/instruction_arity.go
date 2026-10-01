package bytecode

import (
	"fmt"
)

func instructionPayload[T any](inst *Instruction) (T, error) {
	if value, ok := inst.Payload.(T); ok {
		return value, nil
	}
	var value T
	return value, fmt.Errorf("instruction payload %T does not match %T", inst.Payload, value)
}

func instructionArity(inst *Instruction) (inputs, outputs int, err error) {
	if !IsKnownOpcode(inst.Op) {
		return 0, 0, fmt.Errorf("unknown opcode %q", inst.Op)
	}
	arity := &opcodeSpecs[int(inst.Op)-1].Arity
	switch arity.Kind {
	case "fixed":
		return arity.Inputs, arity.Outputs, nil
	case "payload":
	default:
		return 0, 0, fmt.Errorf("missing operand arity for %q", inst.Op)
	}
	switch inst.Op {
	case OpAppend:
		payload, err := instructionPayload[CountPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		need := payload.Count + 1
		return need, 1, nil
	case OpMakeSequence:
		payload, err := instructionPayload[MakeSequencePayload](inst)
		if err != nil {
			return 0, 0, err
		}
		return payload.ElementCount, 1, nil
	case OpMakeMap:
		payload, err := instructionPayload[MakeMapPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		need := payload.EntryCount * 2
		if payload.HasCapacity {
			need++
		}
		return need, 1, nil
	case OpMakeStruct:
		payload, err := instructionPayload[MakeStructPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		need := len(payload.Fields)
		return need, 1, nil
	case OpMakeSlice:
		payload, err := instructionPayload[MakeSlicePayload](inst)
		if err != nil {
			return 0, 0, err
		}
		if payload.HasCapacity {
			return 2, 1, nil
		}
		return 1, 1, nil
	case OpReturn:
		payload, err := instructionPayload[ReturnPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		return payload.ResultCount, 0, nil
	case OpCallValue:
		payload, err := instructionPayload[CallPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		need := payload.ArgCount + 1
		return need, payload.ResultCount, nil
	case OpCallDirect:
		payload, err := instructionPayload[CallPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		return payload.ArgCount, payload.ResultCount, nil
	case OpTailCallDirect:
		payload, err := instructionPayload[CallPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		return payload.ArgCount, 0, nil
	case OpCallInterface:
		payload, err := instructionPayload[CallInterfacePayload](inst)
		if err != nil {
			return 0, 0, err
		}
		need := payload.ArgCount + 1
		return need, payload.ResultCount, nil
	case OpSpawn:
		payload, err := instructionPayload[CallPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		need := payload.ArgCount + 1
		return need, 0, nil
	case OpCallFFI:
		payload, err := instructionPayload[CallFFIPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		return payload.ArgCount, payload.ResultCount, nil
	case OpCallIntrinsic:
		payload, err := instructionPayload[CallIntrinsicPayload](inst)
		if err != nil {
			return 0, 0, err
		}
		return payload.ArgCount, payload.ResultCount, nil
	default:
		return 0, 0, fmt.Errorf("unknown opcode %q", inst.Op)
	}
}
