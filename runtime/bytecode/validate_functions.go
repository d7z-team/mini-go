package bytecode

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func validateTypeRef(path string, ref types.TypeRef, table *types.TypeTable) error {
	if !ref.Valid() {
		return newValidationError(path, fmt.Errorf("invalid type reference %s", types.Format(ref)))
	}
	if ref.Node != "" {
		if table == nil {
			return newValidationError(path, errors.New("type reference has no type table"))
		}
		if _, ok := table.Node(ref); !ok {
			return unknownValidationError(path, fmt.Errorf("unknown type node %q", ref.Node))
		}
	}
	return nil
}

func validateFunctionSignature(path string, signature types.FunctionSignature, table *types.TypeTable) error {
	for i, param := range signature.Params {
		if err := validateTypeRef(fmt.Sprintf("%s.params[%d]", path, i), param.Type, table); err != nil {
			return err
		}
		if signature.Variadic && i == len(signature.Params)-1 {
			underlying := table.Underlying(param.Type)
			node, ok := table.Node(underlying)
			if !ok || node.Kind != types.Slice {
				return newValidationError(path+".variadic", errors.New("variadic signature requires final slice parameter"))
			}
		}
	}
	for i, result := range signature.Results {
		if err := validateTypeRef(fmt.Sprintf("%s.results[%d]", path, i), result, table); err != nil {
			return err
		}
	}
	return nil
}

func validateFunctionBody(path string, fn Function, refs artifactRefs, table *types.TypeTable) (FunctionAnalysis, error) {
	if err := validateFunctionSignature(path+".signature", fn.Signature, table); err != nil {
		return FunctionAnalysis{}, err
	}
	for i, local := range fn.Locals {
		if err := validateTypeRef(fmt.Sprintf("%s.locals[%d].type", path, i), local.Type, table); err != nil {
			return FunctionAnalysis{}, err
		}
	}
	for i, upvalue := range fn.Upvalues {
		if err := validateTypeRef(fmt.Sprintf("%s.upvalues[%d].type", path, i), upvalue.Type, table); err != nil {
			return FunctionAnalysis{}, err
		}
	}
	labels := make(map[string]int)
	locals := collectIDs(len(fn.Locals), func(i int) string { return fn.Locals[i].ID })
	if err := validateResultLocals(path+".result_locals", fn, locals); err != nil {
		return FunctionAnalysis{}, err
	}
	if err := validateUniqueIDs(path+".upvalues", len(fn.Upvalues), func(i int) string { return fn.Upvalues[i].ID }); err != nil {
		return FunctionAnalysis{}, err
	}
	upvalues := collectIDs(len(fn.Upvalues), func(i int) string { return fn.Upvalues[i].ID })
	for j, inst := range fn.Instructions {
		instPath := fmt.Sprintf("%s.instructions[%d]", path, j)
		if err := validateInstruction(instPath, inst); err != nil {
			return FunctionAnalysis{}, err
		}
		if err := validateInstructionRefs(instPath, inst, refs, locals, upvalues, table); err != nil {
			return FunctionAnalysis{}, err
		}
		if inst.Op == string(OpReturn) {
			var payload ReturnPayload
			if err := decodePayload(instPath, inst.Payload, &payload); err != nil {
				return FunctionAnalysis{}, err
			}
			if payload.ResultCount != len(fn.Signature.Results) {
				return FunctionAnalysis{}, schemaMismatchValidationError(instPath+".payload.result_count", fmt.Errorf("return result count mismatch: got %d, want %d", payload.ResultCount, len(fn.Signature.Results)))
			}
		}
		if inst.Op == string(OpTailCallDirect) {
			var payload CallPayload
			if err := decodePayload(instPath, inst.Payload, &payload); err != nil {
				return FunctionAnalysis{}, err
			}
			if payload.ResultCount != len(fn.Signature.Results) {
				return FunctionAnalysis{}, schemaMismatchValidationError(instPath+".payload.result_count", fmt.Errorf("tail call result count mismatch: got %d, want %d", payload.ResultCount, len(fn.Signature.Results)))
			}
		}
		if inst.Op == string(OpLabel) {
			var payload LabelPayload
			if err := decodePayload(instPath, inst.Payload, &payload); err != nil {
				return FunctionAnalysis{}, err
			}
			label := strings.TrimSpace(payload.Label)
			if label == "" {
				return FunctionAnalysis{}, missingValidationError(instPath+".payload.label", errors.New("missing label"))
			}
			if prev, ok := labels[label]; ok {
				return FunctionAnalysis{}, newCodedValidationError(ValidationLabelDuplicate, instPath+".payload.label", fmt.Errorf("duplicate label %q first seen at instruction %d", label, prev))
			}
			labels[label] = j
		}
	}
	for j, inst := range fn.Instructions {
		instPath := fmt.Sprintf("%s.instructions[%d]", path, j)
		switch inst.Op {
		case string(OpJump), string(OpJumpIf):
			var payload JumpPayload
			if err := decodePayload(instPath, inst.Payload, &payload); err != nil {
				return FunctionAnalysis{}, err
			}
			label := strings.TrimSpace(payload.Label)
			if label == "" {
				return FunctionAnalysis{}, missingValidationError(instPath+".payload.label", errors.New("missing jump label"))
			}
			if _, ok := labels[label]; !ok {
				return FunctionAnalysis{}, newCodedValidationError(ValidationLabelUnknown, instPath+".payload.label", fmt.Errorf("unknown label %q", label))
			}
		}
	}
	analysis, err := AnalyzeFunction(fn)
	if err != nil {
		if validationErr, ok := err.(ValidationError); ok {
			return FunctionAnalysis{}, newCodedValidationError(validationErr.Code, path+"."+validationErr.Path, validationErr.Err)
		}
		return FunctionAnalysis{}, newValidationError(path+".instructions", err)
	}
	return analysis, nil
}

func validateResultLocals(path string, fn Function, locals map[string]struct{}) error {
	if len(fn.ResultLocals) == 0 {
		return nil
	}
	if len(fn.ResultLocals) != len(fn.Signature.Results) {
		return schemaMismatchValidationError(path, fmt.Errorf("result local count mismatch: got %d, want %d", len(fn.ResultLocals), len(fn.Signature.Results)))
	}
	seen := make(map[string]struct{}, len(fn.ResultLocals))
	for i, local := range fn.ResultLocals {
		local = strings.TrimSpace(local)
		if local == "" {
			return missingValidationError(fmt.Sprintf("%s[%d]", path, i), errors.New("missing result local"))
		}
		if _, ok := locals[local]; !ok {
			return unknownValidationError(fmt.Sprintf("%s[%d]", path, i), fmt.Errorf("unknown local slot %q", local))
		}
		if _, ok := seen[local]; ok {
			return newValidationError(fmt.Sprintf("%s[%d]", path, i), fmt.Errorf("duplicate result local %q", local))
		}
		seen[local] = struct{}{}
	}
	return nil
}

func validateInstructionRefs(path string, inst Instruction, refs artifactRefs, locals, upvalues map[string]struct{}, table *types.TypeTable) error {
	switch inst.Op {
	case string(OpConst):
		var payload ConstPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if _, ok := refs.constants[payload.Constant]; !ok {
			return unknownValidationError(path+".payload.constant", fmt.Errorf("unknown constant id %q", payload.Constant))
		}
		if refs.untypedConstants[payload.Constant] {
			return newValidationError(path+".payload.constant", fmt.Errorf("untyped constant %q cannot be used by runtime instructions", payload.Constant))
		}
	case string(OpSelect):
		var payload SelectPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		used := []string{payload.Index}
		for _, selected := range payload.Cases {
			used = append(used, selected.Channel)
			if selected.Send != "" {
				used = append(used, selected.Send)
			} else {
				used = append(used, selected.Value, selected.OK)
			}
		}
		for _, local := range used {
			if _, ok := locals[local]; !ok {
				return unknownValidationError(path+".payload", fmt.Errorf("unknown select local %q", local))
			}
		}
	case string(OpLoadLocal), string(OpStoreLocal), string(OpMapIterInit), string(OpMapIterNext), string(OpMapIterClose):
		var payload LocalPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.Rebind && inst.Op != string(OpStoreLocal) {
			return newValidationError(path+".payload.rebind", errors.New("rebind is only valid for store_local"))
		}
		if _, ok := locals[payload.Local]; !ok {
			return unknownValidationError(path+".payload.local", fmt.Errorf("unknown local %q", payload.Local))
		}
	case string(OpLoadUpvalue), string(OpStoreUpvalue):
		var payload UpvaluePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if _, ok := upvalues[payload.Upvalue]; !ok {
			return unknownValidationError(path+".payload.upvalue", fmt.Errorf("unknown upvalue %q", payload.Upvalue))
		}
	case string(OpLoadGlobal), string(OpStoreGlobal):
		var payload GlobalPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if _, ok := refs.globals[payload.Global]; !ok {
			return unknownValidationError(path+".payload.global", fmt.Errorf("unknown global id %q", payload.Global))
		}
	case string(OpAddressOf):
		var payload AddressPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if err := validateAddressRef(path+".payload", payload, refs, locals, upvalues); err != nil {
			return err
		}
	case string(OpCallDirect), string(OpTailCallDirect):
		var payload CallPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		modulePath := strings.TrimSpace(payload.ModulePath)
		if modulePath != "" && modulePath != refs.modulePath {
			if _, ok := refs.moduleExports[modulePath]; !ok {
				return unknownValidationError(path+".payload.module_path", fmt.Errorf("unknown module requirement %q", modulePath))
			}
		} else {
			signature, ok := refs.functionSignatures[payload.Function]
			if !ok {
				return unknownValidationError(path+".payload.function", fmt.Errorf("unknown function id %q", payload.Function))
			}
			if err := validateCallCounts(path+".payload", payload.ArgCount, payload.ResultCount, signature); err != nil {
				return fmt.Errorf("call %s: %w", payload.Function, err)
			}
		}
	case string(OpCallInterface):
		var payload CallInterfacePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		underlying := table.Underlying(payload.InterfaceType)
		node, ok := table.Node(underlying)
		if !ok || node.Kind != types.Interface {
			return newValidationError(path+".payload.interface_type", errors.New("interface call requires an interface type"))
		}
		for _, method := range node.Methods {
			if method.Name == payload.Method {
				return validateCallCounts(path+".payload", payload.ArgCount, payload.ResultCount, method.Signature)
			}
		}
		// Imported and promoted method sets are linked outside the package-local
		// type table. Validate the shape when the method is present; runtime
		// dispatch validates the resolved method before entering a frame.
		return nil
	case string(OpMakeClosure):
		var payload ClosurePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		modulePath := strings.TrimSpace(payload.ModulePath)
		if modulePath != "" && modulePath != refs.modulePath {
			if _, ok := refs.moduleExports[modulePath]; !ok {
				return unknownValidationError(path+".payload.module_path", fmt.Errorf("unknown module requirement %q", modulePath))
			}
		} else if _, ok := refs.functions[payload.Function]; !ok {
			return unknownValidationError(path+".payload.function", fmt.Errorf("unknown function id %q", payload.Function))
		}
		if modulePath == "" || modulePath == refs.modulePath {
			if want := refs.functionUpvalues[payload.Function]; want != len(payload.Captures) {
				return schemaMismatchValidationError(path+".payload.captures", fmt.Errorf("capture count mismatch for %q: got %d, want %d", payload.Function, len(payload.Captures), want))
			}
		}
		for i, capture := range payload.Captures {
			if err := validateAddressRef(fmt.Sprintf("%s.payload.captures[%d]", path, i), capture, refs, locals, upvalues); err != nil {
				return err
			}
		}
	case string(OpCallFFI):
		var payload CallFFIPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if payload.ArgCount != 2 || payload.ResultCount != 3 {
			return schemaMismatchValidationError(path+".payload", errors.New("invalid FFI stack contract"))
		}
	case string(OpCallIntrinsic):
		var payload CallIntrinsicPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		descriptor, ok := Intrinsic(payload.ID)
		if !ok || descriptor.ArgCount != payload.ArgCount || descriptor.ResultCount != payload.ResultCount {
			return newValidationError(path+".payload.id", fmt.Errorf("invalid intrinsic contract %q", payload.ID))
		}
	case string(OpLoadExport):
		var payload ExportPayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		exports, ok := refs.moduleExports[payload.ModulePath]
		if !ok {
			return unknownValidationError(path+".payload.module_path", fmt.Errorf("unknown module requirement %q", payload.ModulePath))
		}
		if _, ok := exports[payload.Export]; !ok {
			return unknownValidationError(path+".payload.export", fmt.Errorf("unknown module export %q", payload.Export))
		}
	case string(OpInitModule):
		var payload InitModulePayload
		if err := decodePayload(path, inst.Payload, &payload); err != nil {
			return err
		}
		if _, ok := refs.moduleExports[payload.ModulePath]; !ok {
			return unknownValidationError(path+".payload.module_path", fmt.Errorf("unknown module requirement %q", payload.ModulePath))
		}
	}
	return nil
}

func validateCallCounts(path string, arguments, results int, signature types.FunctionSignature) error {
	if arguments != len(signature.Params) {
		return schemaMismatchValidationError(path+".arg_count", fmt.Errorf("call argument count mismatch: got %d, want %d", arguments, len(signature.Params)))
	}
	if results != len(signature.Results) {
		return schemaMismatchValidationError(path+".result_count", fmt.Errorf("call result count mismatch: got %d, want %d", results, len(signature.Results)))
	}
	return nil
}

func validateAddressRef(path string, payload AddressPayload, refs artifactRefs, locals, upvalues map[string]struct{}) error {
	switch payload.Kind {
	case "export":
		exports, ok := refs.moduleExports[payload.ModulePath]
		if !ok {
			return unknownValidationError(path+".module_path", fmt.Errorf("unknown module requirement %q", payload.ModulePath))
		}
		if _, ok := exports[payload.Export]; !ok {
			return unknownValidationError(path+".export", fmt.Errorf("unknown module export %q", payload.Export))
		}
	case "local":
		if _, ok := locals[payload.Local]; !ok {
			return unknownValidationError(path+".local", fmt.Errorf("unknown local %q", payload.Local))
		}
	case "upvalue":
		if _, ok := upvalues[payload.Upvalue]; !ok {
			return unknownValidationError(path+".upvalue", fmt.Errorf("unknown upvalue slot %q", payload.Upvalue))
		}
	case "global":
		if _, ok := refs.globals[payload.Global]; !ok {
			return unknownValidationError(path+".global", fmt.Errorf("unknown global id %q", payload.Global))
		}
	default:
		return unsupportedValueValidationError(path+".kind", fmt.Errorf("unsupported address kind %q", payload.Kind))
	}
	return validateAddressPathRefs(path+".path", payload.Path, locals)
}

func validateAddressPathRefs(path string, segments []AddressPathSegment, locals map[string]struct{}) error {
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
			if _, ok := locals[segment.Local]; !ok {
				return unknownValidationError(segmentPath+".local", fmt.Errorf("unknown local %q", segment.Local))
			}
		default:
			return unsupportedValueValidationError(segmentPath+".kind", fmt.Errorf("unsupported address path segment kind %q", segment.Kind))
		}
	}
	return nil
}

func validateUniqueIDs(section string, n int, idAt func(int) string) error {
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		id := strings.TrimSpace(idAt(i))
		if id == "" {
			return missingValidationError(fmt.Sprintf("%s[%d].id", section, i), errors.New("missing id"))
		}
		if prev, ok := seen[id]; ok {
			return newCodedValidationError(ValidationIDDuplicate, fmt.Sprintf("%s[%d].id", section, i), fmt.Errorf("duplicate id %q first seen at %s[%d]", id, section, prev))
		}
		seen[id] = i
	}
	return nil
}

func collectIDs(n int, idAt func(int) string) map[string]struct{} {
	out := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := strings.TrimSpace(idAt(i))
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func collectFunctionUpvalueCounts(functions []Function) map[string]int {
	out := make(map[string]int, len(functions))
	for _, fn := range functions {
		out[fn.ID] = len(fn.Upvalues)
	}
	return out
}

func collectFunctionInstructionCounts(functions []Function) map[string]int {
	out := make(map[string]int, len(functions))
	for _, fn := range functions {
		out[fn.ID] = len(fn.Instructions)
	}
	return out
}
