package bytecode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func validateInstruction(path string, inst *Instruction) error {
	if inst.Op == 0 {
		return missingValidationError(path+".op", errors.New("missing op"))
	}
	if !IsKnownOpcode(inst.Op) {
		return newCodedValidationError(ValidationOpcodeUnknown, path+".op", fmt.Errorf("unknown opcode %q", inst.Op))
	}
	return validateInstructionPayload(path, inst)
}

func validateInstructionPayload(path string, inst *Instruction) error {
	spec, ok := OpcodeSpecFor(inst.Op)
	if !ok {
		return nil
	}
	if spec.Payload == "" {
		if inst.Payload != nil {
			return newCodedValidationError(ValidationPayloadUnexpected, path+".payload", fmt.Errorf("opcode %q must not have payload", inst.Op))
		}
		return nil
	}
	switch inst.Op {
	case OpGetPath:
		payload, err := validationPayload[FieldPathPayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() || len(payload.Fields) == 0 || len(payload.Fields) > 16 {
			return newValidationError(path, errors.New("field path requires a root type and 1 to 16 fields"))
		}
	case OpCompareBranch:
		payload, err := validationPayload[CompareBranchPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.Label == "" || !payload.Type.Valid() {
			return missingValidationError(path, errors.New("comparison requires a type and target"))
		}
		switch payload.Operator {
		case "==", "!=", "<", "<=", ">", ">=":
		default:
			return newValidationError(path, errors.New("invalid comparison operator"))
		}
	case OpTypeDispatch:
		payload, err := validationPayload[TypeDispatchPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.Subject == "" || payload.Default == "" {
			return missingValidationError(path, errors.New("type dispatch requires subject and default label"))
		}
		var nilType types.TypeRef
		for _, match := range payload.Cases {
			if match.Label == "" || (!match.Type.Valid() && (match.Type != nilType || !match.Original)) {
				return newValidationError(path, errors.New("type case requires label and a type or nil binding"))
			}
		}
	case OpSelect:
		payload, err := validationPayload[SelectPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Index) == "" {
			return missingValidationError(path+".payload.index", errors.New("missing select index local"))
		}
		for index, selected := range payload.Cases {
			if strings.TrimSpace(selected.Channel) == "" ||
				(selected.Send == "" && (selected.Value == "" || selected.OK == "")) ||
				(selected.Send != "" && (selected.Value != "" || selected.OK != "")) {
				return newValidationError(path+".payload.cases["+strconv.Itoa(index)+"]", errors.New("select case requires channel and either send or value/ok locals"))
			}
		}
	case OpConst:
		payload, err := validationPayload[ConstPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Constant) == "" {
			return missingValidationError(path+".payload.constant", errors.New("missing constant id"))
		}
	case OpZero, OpTypeAssert, OpTypeAssertOK, OpConvert:
		payload, err := validationPayload[TypePayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
	case OpUnary, OpBinary:
		payload, err := validationPayload[OperatorPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Operator) == "" {
			return missingValidationError(path+".payload.operator", errors.New("missing operator"))
		}
	case OpMakeSequence:
		payload, err := validationPayload[MakeSequencePayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
		if payload.ElementCount < 0 {
			return newValidationError(path+".payload.element_count", errors.New("element count must be non-negative"))
		}
	case OpMakeMap:
		payload, err := validationPayload[MakeMapPayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
		if payload.EntryCount < 0 {
			return newValidationError(path+".payload.entry_count", errors.New("entry count must be non-negative"))
		}
	case OpMakeStruct:
		payload, err := validationPayload[MakeStructPayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
		seen := make(map[string]struct{}, len(payload.Fields))
		for i, field := range payload.Fields {
			field = strings.TrimSpace(field)
			if field == "" {
				return missingValidationError(path+".payload.fields["+strconv.Itoa(i)+"]", errors.New("missing field"))
			}
			if _, exists := seen[field]; exists {
				return newValidationError(path+".payload.fields["+strconv.Itoa(i)+"]", fmt.Errorf("duplicate struct field %q", field))
			}
			seen[field] = struct{}{}
		}
	case OpMakeSlice:
		payload, err := validationPayload[MakeSlicePayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
	case OpMakeWaitable:
		payload, err := validationPayload[MakeWaitablePayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
	case OpAppend:
		payload, err := validationPayload[CountPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.Count < 0 {
			return newValidationError(path+".payload.count", errors.New("count must be non-negative"))
		}
		if payload.Expand && payload.Count != 1 {
			return newValidationError(path+".payload.count", errors.New("expanded append requires exactly one source argument"))
		}
	case OpLoadField, OpStoreField:
		payload, err := validationPayload[FieldPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Field) == "" {
			return missingValidationError(path+".payload.field", errors.New("missing field"))
		}
	case OpLoadExport:
		payload, err := validationPayload[ExportPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.ModulePath) == "" {
			return missingValidationError(path+".payload.module_path", errors.New("missing module path"))
		}
		if strings.TrimSpace(payload.Export) == "" {
			return missingValidationError(path+".payload.export", errors.New("missing export"))
		}
	case OpInitModule:
		payload, err := validationPayload[InitModulePayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.ModulePath) == "" {
			return missingValidationError(path+".payload.module_path", errors.New("missing module path"))
		}
	case OpLoadLocal, OpStoreLocal, OpMapIterInit, OpMapIterNext, OpMapIterClose:
		payload, err := validationPayload[LocalPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Local) == "" {
			return missingValidationError(path+".payload.local", errors.New("missing local id"))
		}
	case OpLoadUpvalue, OpStoreUpvalue:
		payload, err := validationPayload[UpvaluePayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Upvalue) == "" {
			return missingValidationError(path+".payload.upvalue", errors.New("missing upvalue id"))
		}
	case OpLoadGlobal, OpStoreGlobal:
		payload, err := validationPayload[GlobalPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Global) == "" {
			return missingValidationError(path+".payload.global", errors.New("missing global id"))
		}
	case OpAddressOf:
		payload, err := validationPayload[AddressPayload](path, inst)
		if err != nil {
			return err
		}
		switch payload.Kind {
		case "export":
			if strings.TrimSpace(payload.ModulePath) == "" || strings.TrimSpace(payload.Export) == "" {
				return missingValidationError(path+".payload", errors.New("export address requires module path and export name"))
			}
			if payload.Local != "" || payload.Upvalue != "" || payload.Global != "" {
				return newValidationError(path+".payload", errors.New("export address cannot contain local, upvalue, or global ids"))
			}
		case "local":
			if strings.TrimSpace(payload.Local) == "" {
				return missingValidationError(path+".payload.local", errors.New("missing local id"))
			}
		case "upvalue":
			if strings.TrimSpace(payload.Upvalue) == "" {
				return missingValidationError(path+".payload.upvalue", errors.New("missing upvalue id"))
			}
		case "global":
			if strings.TrimSpace(payload.Global) == "" {
				return missingValidationError(path+".payload.global", errors.New("missing global id"))
			}
		default:
			return unsupportedValueValidationError(path+".payload.kind", fmt.Errorf("unsupported address kind %q", payload.Kind))
		}
		if payload.Kind != "export" && (payload.ModulePath != "" || payload.Export != "") {
			return newValidationError(path+".payload", errors.New("local address cannot contain export identity"))
		}
		if err := validateAddressPath(path+".payload.path", payload.Path, nil); err != nil {
			return err
		}
	case OpLabel:
		payload, err := validationPayload[LabelPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Label) == "" {
			return missingValidationError(path+".payload.label", errors.New("missing label"))
		}
	case OpJump, OpJumpIf:
		payload, err := validationPayload[JumpPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Label) == "" {
			return missingValidationError(path+".payload.label", errors.New("missing jump label"))
		}
	case OpCallDirect, OpTailCallDirect:
		payload, err := validationPayload[CallPayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Function) == "" {
			return missingValidationError(path+".payload.function", errors.New("missing function id"))
		}
		if payload.ArgCount < 0 {
			return newValidationError(path+".payload.arg_count", errors.New("arg count must be non-negative"))
		}
		if payload.ResultCount < 0 {
			return newValidationError(path+".payload.result_count", errors.New("result count must be non-negative"))
		}
	case OpCallInterface:
		payload, err := validationPayload[CallInterfacePayload](path, inst)
		if err != nil {
			return err
		}
		if !payload.InterfaceType.Valid() {
			return missingValidationError(path+".payload.interface_type", errors.New("missing interface type"))
		}
		if strings.TrimSpace(payload.Method) == "" {
			return missingValidationError(path+".payload.method", errors.New("missing method"))
		}
		if payload.ArgCount < 0 {
			return newValidationError(path+".payload.arg_count", errors.New("arg count must be non-negative"))
		}
		if payload.ResultCount < 0 {
			return newValidationError(path+".payload.result_count", errors.New("result count must be non-negative"))
		}
	case OpMakeClosure:
		payload, err := validationPayload[ClosurePayload](path, inst)
		if err != nil {
			return err
		}
		if strings.TrimSpace(payload.Function) == "" {
			return missingValidationError(path+".payload.function", errors.New("missing function id"))
		}
		for i, capture := range payload.Captures {
			capturePath := path + ".payload.captures[" + strconv.Itoa(i) + "]"
			if capture.ModulePath != "" || capture.Export != "" {
				return newValidationError(capturePath, errors.New("closure capture cannot contain export identity"))
			}
			switch capture.Kind {
			case "local":
				if strings.TrimSpace(capture.Local) == "" {
					return missingValidationError(capturePath+".local", errors.New("missing local id"))
				}
			case "upvalue":
				if strings.TrimSpace(capture.Upvalue) == "" {
					return missingValidationError(capturePath+".upvalue", errors.New("missing upvalue id"))
				}
			case "global":
				if strings.TrimSpace(capture.Global) == "" {
					return missingValidationError(capturePath+".global", errors.New("missing global id"))
				}
			default:
				return unsupportedValueValidationError(capturePath+".kind", fmt.Errorf("unsupported address kind %q", capture.Kind))
			}
		}
	case OpCallValue, OpSpawn:
		payload, err := validationPayload[CallPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.ArgCount < 0 {
			return newValidationError(path+".payload.arg_count", errors.New("arg count must be non-negative"))
		}
		if payload.ResultCount < 0 {
			return newValidationError(path+".payload.result_count", errors.New("result count must be non-negative"))
		}
		if inst.Op == OpSpawn && payload.ResultCount != 0 {
			return newValidationError(path+".payload.result_count", errors.New("spawn must not produce results"))
		}
	case OpReturn:
		payload, err := validationPayload[ReturnPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.ResultCount < 0 {
			return newValidationError(path+".payload.result_count", errors.New("return result count must be non-negative"))
		}
	case OpDeferPush:
		payload, err := validationPayload[DeferPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.OwnerDepth < 0 {
			return newValidationError(path+".payload.owner_depth", errors.New("owner depth must be non-negative"))
		}
	case OpCallFFI:
		payload, err := validationPayload[CallFFIPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.ArgCount != 2 {
			return newValidationError(path+".payload.arg_count", errors.New("FFI call requires route and payload arguments"))
		}
		if payload.ResultCount != 3 {
			return newValidationError(path+".payload.result_count", errors.New("FFI call requires payload, message, and status results"))
		}
	case OpCallIntrinsic:
		payload, err := validationPayload[CallIntrinsicPayload](path, inst)
		if err != nil {
			return err
		}
		descriptor, ok := Intrinsic(payload.ID)
		if !ok {
			return unknownValidationError(path+".payload.id", fmt.Errorf("unknown intrinsic %q", payload.ID))
		}
		if payload.ArgCount != descriptor.ArgCount {
			return schemaMismatchValidationError(path+".payload.arg_count", fmt.Errorf("intrinsic %q arg count mismatch: got %d, want %d", payload.ID, payload.ArgCount, descriptor.ArgCount))
		}
		if payload.ResultCount != descriptor.ResultCount {
			return schemaMismatchValidationError(path+".payload.result_count", fmt.Errorf("intrinsic %q result count mismatch: got %d, want %d", payload.ID, payload.ResultCount, descriptor.ResultCount))
		}
	}
	return nil
}

// A nil local table validates shape only; an empty table rejects every index reference.
func validateAddressPath(path string, segments []AddressPathSegment, locals map[string]types.TypeRef) error {
	for i, segment := range segments {
		segmentPath := path + "[" + strconv.Itoa(i) + "]"
		switch strings.TrimSpace(segment.Kind) {
		case "indirect":
		case "field":
			if strings.TrimSpace(segment.Field) == "" {
				return missingValidationError(segmentPath+".field", errors.New("missing field"))
			}
		case "index":
			if strings.TrimSpace(segment.Local) == "" {
				return missingValidationError(segmentPath+".local", errors.New("missing local id"))
			}
			if locals != nil {
				if _, ok := locals[segment.Local]; !ok {
					return unknownValidationError(segmentPath+".local", fmt.Errorf("unknown local %q", segment.Local))
				}
			}
		default:
			return unsupportedValueValidationError(segmentPath+".kind", fmt.Errorf("unsupported address path segment kind %q", segment.Kind))
		}
	}
	return nil
}

func validationPayload[T any](path string, inst *Instruction) (T, error) {
	value, err := instructionPayload[T](inst)
	if err != nil {
		return value, newValidationError(path+".payload", err)
	}
	return value, nil
}
