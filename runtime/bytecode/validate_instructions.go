package bytecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func validateInstruction(path string, inst Instruction) error {
	if strings.TrimSpace(inst.Op) == "" {
		return missingValidationError(path+".op", errors.New("missing op"))
	}
	if !IsKnownOpcode(inst.Op) {
		return newCodedValidationError(ValidationOpcodeUnknown, path+".op", fmt.Errorf("unknown opcode %q", inst.Op))
	}
	if len(inst.Payload) > 0 && !json.Valid([]byte(inst.Payload)) {
		return newCodedValidationError(ValidationPayloadInvalidJSON, path+".payload", errors.New("invalid json payload"))
	}
	return validateInstructionPayload(path, inst)
}

func validateInstructionPayload(path string, inst Instruction) error {
	spec, ok := OpcodeSpecFor(inst.Op)
	if !ok {
		return nil
	}
	if spec.Payload == "" {
		payload := strings.TrimSpace(string(inst.Payload))
		if payload != "" && payload != "{}" {
			return newCodedValidationError(ValidationPayloadUnexpected, path+".payload", fmt.Errorf("opcode %q must not have payload", inst.Op))
		}
		return nil
	}
	switch inst.Op {
	case string(OpSelect):
		var payload SelectPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Index) == "" {
			return missingValidationError(path+".payload.index", errors.New("missing select index local"))
		}
		for index, selected := range payload.Cases {
			if strings.TrimSpace(selected.Channel) == "" ||
				(selected.Send == "" && (selected.Value == "" || selected.OK == "")) ||
				(selected.Send != "" && (selected.Value != "" || selected.OK != "")) {
				return newValidationError(fmt.Sprintf("%s.payload.cases[%d]", path, index), errors.New("select case requires channel and either send or value/ok locals"))
			}
		}
	case string(OpConst):
		var payload ConstPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Constant) == "" {
			return missingValidationError(path+".payload.constant", errors.New("missing constant id"))
		}
	case string(OpZero), string(OpTypeAssert), string(OpTypeAssertOK), string(OpConvert):
		var payload TypePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
	case string(OpUnary), string(OpBinary):
		var payload OperatorPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Operator) == "" {
			return missingValidationError(path+".payload.operator", errors.New("missing operator"))
		}
	case string(OpMakeSequence):
		var payload MakeSequencePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
		if payload.ElementCount < 0 {
			return newValidationError(path+".payload.element_count", errors.New("element count must be non-negative"))
		}
	case string(OpMakeMap):
		var payload MakeMapPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
		if payload.EntryCount < 0 {
			return newValidationError(path+".payload.entry_count", errors.New("entry count must be non-negative"))
		}
	case string(OpMakeStruct):
		var payload MakeStructPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
		seen := make(map[string]struct{}, len(payload.Fields))
		for i, field := range payload.Fields {
			field = strings.TrimSpace(field)
			if field == "" {
				return missingValidationError(fmt.Sprintf("%s.payload.fields[%d]", path, i), errors.New("missing field"))
			}
			if _, exists := seen[field]; exists {
				return newValidationError(fmt.Sprintf("%s.payload.fields[%d]", path, i), fmt.Errorf("duplicate struct field %q", field))
			}
			seen[field] = struct{}{}
		}
	case string(OpMakeSlice):
		var payload MakeSlicePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
	case string(OpMakeWaitable):
		var payload MakeWaitablePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if !payload.Type.Valid() {
			return missingValidationError(path+".payload.type", errors.New("missing type"))
		}
	case string(OpAppend):
		var payload CountPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.Count < 0 {
			return newValidationError(path+".payload.count", errors.New("count must be non-negative"))
		}
		if payload.Expand && payload.Count != 1 {
			return newValidationError(path+".payload.count", errors.New("expanded append requires exactly one source argument"))
		}
	case string(OpLoadField), string(OpStoreField):
		var payload FieldPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Field) == "" {
			return missingValidationError(path+".payload.field", errors.New("missing field"))
		}
	case string(OpLoadExport):
		var payload ExportPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.ModulePath) == "" {
			return missingValidationError(path+".payload.module_path", errors.New("missing module path"))
		}
		if strings.TrimSpace(payload.Export) == "" {
			return missingValidationError(path+".payload.export", errors.New("missing export"))
		}
	case string(OpInitModule):
		var payload InitModulePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.ModulePath) == "" {
			return missingValidationError(path+".payload.module_path", errors.New("missing module path"))
		}
	case string(OpLoadLocal), string(OpStoreLocal), string(OpMapIterInit), string(OpMapIterNext), string(OpMapIterClose):
		var payload LocalPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Local) == "" {
			return missingValidationError(path+".payload.local", errors.New("missing local id"))
		}
	case string(OpLoadUpvalue), string(OpStoreUpvalue):
		var payload UpvaluePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Upvalue) == "" {
			return missingValidationError(path+".payload.upvalue", errors.New("missing upvalue id"))
		}
	case string(OpLoadGlobal), string(OpStoreGlobal):
		var payload GlobalPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Global) == "" {
			return missingValidationError(path+".payload.global", errors.New("missing global id"))
		}
	case string(OpAddressOf):
		var payload AddressPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
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
		if err := validateAddressPathShape(path+".payload.path", payload.Path); err != nil {
			return err
		}
	case string(OpLabel):
		var payload LabelPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Label) == "" {
			return missingValidationError(path+".payload.label", errors.New("missing label"))
		}
	case string(OpJump), string(OpJumpIf):
		var payload JumpPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Label) == "" {
			return missingValidationError(path+".payload.label", errors.New("missing jump label"))
		}
	case string(OpCallDirect), string(OpTailCallDirect):
		var payload CallPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
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
	case string(OpCallInterface):
		var payload CallInterfacePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
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
	case string(OpMakeClosure):
		var payload ClosurePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Function) == "" {
			return missingValidationError(path+".payload.function", errors.New("missing function id"))
		}
		for i, capture := range payload.Captures {
			capturePath := fmt.Sprintf("%s.payload.captures[%d]", path, i)
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
	case string(OpCallValue), string(OpSpawn):
		var payload CallPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.ArgCount < 0 {
			return newValidationError(path+".payload.arg_count", errors.New("arg count must be non-negative"))
		}
		if payload.ResultCount < 0 {
			return newValidationError(path+".payload.result_count", errors.New("result count must be non-negative"))
		}
		if inst.Op == string(OpSpawn) && payload.ResultCount != 0 {
			return newValidationError(path+".payload.result_count", errors.New("spawn must not produce results"))
		}
	case string(OpReturn):
		var payload ReturnPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.ResultCount < 0 {
			return newValidationError(path+".payload.result_count", errors.New("return result count must be non-negative"))
		}
	case string(OpDeferPush):
		payloadText := strings.TrimSpace(string(inst.Payload))
		if payloadText == "" {
			return nil
		}
		var payload DeferPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.OwnerDepth < 0 {
			return newValidationError(path+".payload.owner_depth", errors.New("owner depth must be non-negative"))
		}
	case string(OpCallFFI):
		var payload CallFFIPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.ArgCount != 2 {
			return newValidationError(path+".payload.arg_count", errors.New("FFI call requires route and payload arguments"))
		}
		if payload.ResultCount != 3 {
			return newValidationError(path+".payload.result_count", errors.New("FFI call requires payload, message, and status results"))
		}
	case string(OpCallIntrinsic):
		var payload CallIntrinsicPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
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

func validateAddressPathShape(path string, segments []AddressPathSegment) error {
	for i, segment := range segments {
		segmentPath := fmt.Sprintf("%s[%d]", path, i)
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
		default:
			return unsupportedValueValidationError(segmentPath+".kind", fmt.Errorf("unsupported address path segment kind %q", segment.Kind))
		}
	}
	return nil
}

func decodePayload(path string, raw json.RawMessage, out any) error {
	if err := DecodeInstructionPayload(raw, out); err != nil {
		return newValidationError(path+".payload", err)
	}
	return nil
}
