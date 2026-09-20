package bytecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type FunctionAnalysis struct {
	MaxStack int
}

func AnalyzeFunction(fn Function) (FunctionAnalysis, error) {
	labels := make(map[string]int)
	for i, inst := range fn.Instructions {
		if inst.Op != string(OpLabel) {
			continue
		}
		var payload LabelPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return FunctionAnalysis{}, newValidationError(fmt.Sprintf("instructions[%d]", i), err)
		}
		label := strings.TrimSpace(payload.Label)
		if label == "" {
			return FunctionAnalysis{}, missingValidationError(fmt.Sprintf("instructions[%d].payload.label", i), errors.New("missing label"))
		}
		if previous, exists := labels[label]; exists {
			return FunctionAnalysis{}, newCodedValidationError(ValidationLabelDuplicate, fmt.Sprintf("instructions[%d].payload.label", i), fmt.Errorf("duplicate label %q first seen at instruction %d", label, previous))
		}
		labels[label] = i
	}

	if len(fn.Instructions) == 0 {
		return FunctionAnalysis{}, nil
	}
	entry := make([]int, len(fn.Instructions))
	for i := range entry {
		entry[i] = -1
	}
	entry[0] = 0
	queue := []int{0}
	maxStack := 0
	for len(queue) != 0 {
		i := queue[0]
		queue = queue[1:]
		stack := entry[i]
		inst := fn.Instructions[i]
		path := fmt.Sprintf("instructions[%d]", i)
		required, delta, ends, err := instructionStackEffect(inst)
		if err != nil {
			return FunctionAnalysis{}, newValidationError(path, err)
		}
		if stack < required {
			return FunctionAnalysis{}, newCodedValidationError(ValidationStackUnderflow, path, fmt.Errorf("stack underflow: need %d values, have %d", required, stack))
		}
		stack += delta
		if stack > maxStack {
			maxStack = stack
		}
		if ends {
			continue
		}

		successors := make([]int, 0, 2)
		switch inst.Op {
		case string(OpJump):
			target, err := jumpTarget(inst, labels, path)
			if err != nil {
				return FunctionAnalysis{}, err
			}
			successors = append(successors, target)
		case string(OpJumpIf):
			target, err := jumpTarget(inst, labels, path)
			if err != nil {
				return FunctionAnalysis{}, err
			}
			successors = append(successors, target)
			if i+1 < len(fn.Instructions) {
				successors = append(successors, i+1)
			}
		default:
			if i+1 < len(fn.Instructions) {
				successors = append(successors, i+1)
			}
		}
		for _, successor := range successors {
			if entry[successor] == -1 {
				entry[successor] = stack
				queue = append(queue, successor)
				continue
			}
			if entry[successor] != stack {
				return FunctionAnalysis{}, schemaMismatchValidationError(
					fmt.Sprintf("instructions[%d]", successor),
					fmt.Errorf("stack depth mismatch at control-flow join: have %d, want %d", entry[successor], stack),
				)
			}
		}
	}
	last := len(fn.Instructions) - 1
	if entry[last] != -1 {
		_, delta, ends, err := instructionStackEffect(fn.Instructions[last])
		if err != nil {
			return FunctionAnalysis{}, newValidationError("instructions", err)
		}
		if !ends && entry[last]+delta != 0 {
			return FunctionAnalysis{}, newCodedValidationError(ValidationStackUnbalanced, "instructions", fmt.Errorf("stack not balanced at function end: %d values remain", entry[last]+delta))
		}
	}
	return FunctionAnalysis{MaxStack: maxStack}, nil
}

func jumpTarget(inst Instruction, labels map[string]int, path string) (int, error) {
	var payload JumpPayload
	if err := decodeStackPayload(inst.Payload, &payload); err != nil {
		return 0, newValidationError(path, err)
	}
	label := strings.TrimSpace(payload.Label)
	if target, ok := labels[label]; ok {
		return target, nil
	}
	return 0, newCodedValidationError(ValidationLabelUnknown, path+".payload.label", fmt.Errorf("unknown label %q", label))
}

func instructionStackEffect(inst Instruction) (required, delta int, terminal bool, err error) {
	switch inst.Op {
	case string(OpMapIterInit):
		return 1, -1, false, nil
	case string(OpMapIterNext):
		return 0, 3, false, nil
	case string(OpMapIterClose):
		return 0, 0, false, nil
	case string(OpConst), string(OpZero), string(OpLoadLocal), string(OpLoadUpvalue), string(OpLoadGlobal), string(OpMakeClosure),
		string(OpAddressOf), string(OpRecover):
		return 0, 1, false, nil
	case string(OpPop), string(OpStoreLocal), string(OpStoreUpvalue), string(OpStoreGlobal),
		string(OpDeferPush), string(OpWaitableClose):
		return 1, -1, false, nil
	case string(OpStoreIndirect):
		return 2, -2, false, nil
	case string(OpWaitableSend):
		return 2, -2, false, nil
	case string(OpWaitableTrySend):
		return 2, -1, false, nil
	case string(OpUnary):
		return 1, 0, false, nil
	case string(OpBinary):
		return 2, -1, false, nil
	case string(OpLoadIndirect), string(OpTypeAssert), string(OpConvert), string(OpLen), string(OpCap), string(OpMapKeys):
		return 1, 0, false, nil
	case string(OpTypeAssertOK):
		return 1, 1, false, nil
	case string(OpMakeWaitable):
		return 1, 0, false, nil
	case string(OpWaitableRecv):
		return 1, 0, false, nil
	case string(OpWaitableRecvOK):
		return 1, 1, false, nil
	case string(OpWaitableCanRecv), string(OpWaitableCanSend):
		return 1, 0, false, nil
	case string(OpWaitableTryRecv):
		return 1, 1, false, nil
	case string(OpAppend):
		var payload CountPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		need := payload.Count + 1
		return need, 1 - need, false, nil
	case string(OpSlice):
		return 4, -3, false, nil
	case string(OpLoadField):
		return 1, 0, false, nil
	case string(OpLoadIndex):
		return 2, -1, false, nil
	case string(OpLoadIndexOK):
		return 2, 0, false, nil
	case string(OpStringRuneAt):
		return 2, -1, false, nil
	case string(OpStringNextRuneIndex):
		return 2, -1, false, nil
	case string(OpStoreField):
		return 2, -2, false, nil
	case string(OpStoreIndex):
		return 3, -3, false, nil
	case string(OpDelete):
		return 2, -2, false, nil
	case string(OpClear):
		return 1, -1, false, nil
	case string(OpCopy):
		return 2, -1, false, nil
	case string(OpMakeSequence):
		var payload MakeSequencePayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		return payload.ElementCount, 1 - payload.ElementCount, false, nil
	case string(OpMakeMap):
		var payload MakeMapPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		need := payload.EntryCount * 2
		if payload.HasCapacity {
			need++
		}
		return need, 1 - need, false, nil
	case string(OpMakeStruct):
		var payload MakeStructPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		need := len(payload.Fields)
		return need, 1 - need, false, nil
	case string(OpMakeSlice):
		var payload MakeSlicePayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		if payload.HasCapacity {
			return 2, -1, false, nil
		}
		return 1, 0, false, nil
	case string(OpJumpIf):
		return 1, -1, false, nil
	case string(OpLabel), string(OpJump), string(OpInitModule), string(OpSelect):
		return 0, 0, inst.Op == string(OpJump), nil
	case string(OpPanic):
		return 1, -1, true, nil
	case string(OpReturn):
		var payload ReturnPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		return payload.ResultCount, -payload.ResultCount, true, nil
	case string(OpCallValue):
		var payload CallPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		need := payload.ArgCount + 1
		return need, payload.ResultCount - need, false, nil
	case string(OpCallDirect):
		var payload CallPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		return payload.ArgCount, payload.ResultCount - payload.ArgCount, false, nil
	case string(OpTailCallDirect):
		var payload CallPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		return payload.ArgCount, -payload.ArgCount, true, nil
	case string(OpCallInterface):
		var payload CallInterfacePayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		need := payload.ArgCount + 1
		return need, payload.ResultCount - need, false, nil
	case string(OpSpawn):
		var payload CallPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		need := payload.ArgCount + 1
		return need, -need, false, nil
	case string(OpCallFFI):
		var payload CallFFIPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		return payload.ArgCount, payload.ResultCount - payload.ArgCount, false, nil
	case string(OpCallIntrinsic):
		var payload CallIntrinsicPayload
		if err := decodeStackPayload(inst.Payload, &payload); err != nil {
			return 0, 0, false, err
		}
		return payload.ArgCount, payload.ResultCount - payload.ArgCount, false, nil
	case string(OpLoadExport):
		return 0, 1, false, nil
	default:
		return 0, 0, false, fmt.Errorf("unknown opcode %q", inst.Op)
	}
}

func decodeStackPayload(raw json.RawMessage, out any) error {
	return DecodeInstructionPayload(raw, out)
}
