package bytecode

import (
	"errors"
	"fmt"
	"strconv"
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
		if err := validateTypeRef(path+".params["+strconv.Itoa(i)+"]", param.Type, table); err != nil {
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
		if err := validateTypeRef(path+".results["+strconv.Itoa(i)+"]", result, table); err != nil {
			return err
		}
	}
	return nil
}

func validateFunctionBody(path string, fn Function, refs artifactRefs, table *types.TypeTable) error {
	if err := validateFunctionSignature(path+".signature", fn.Signature, table); err != nil {
		return err
	}
	for i, local := range fn.Locals {
		if err := validateTypeRef(path+".locals["+strconv.Itoa(i)+"].type", local.Type, table); err != nil {
			return err
		}
	}
	for i, upvalue := range fn.Upvalues {
		if err := validateTypeRef(path+".upvalues["+strconv.Itoa(i)+"].type", upvalue.Type, table); err != nil {
			return err
		}
	}
	operations, err := fn.Operations()
	if err != nil {
		if issue, ok := err.(ValidationError); ok {
			return newCodedValidationError(issue.Code, path+".code."+issue.Path, issue.Err)
		}
		return newValidationError(path+".code", err)
	}
	instructions := operations
	labels := make(map[string]int)
	locals := make(map[string]types.TypeRef, len(fn.Locals))
	for _, local := range fn.Locals {
		locals[local.ID] = local.Type
	}
	if err := validateResultLocals(path+".result_locals", fn, locals); err != nil {
		return err
	}
	if err := validateUniqueIDs(path+".upvalues", len(fn.Upvalues), func(i int) string { return fn.Upvalues[i].ID }); err != nil {
		return err
	}
	upvalues := collectIDs(len(fn.Upvalues), func(i int) string { return fn.Upvalues[i].ID })
	for j := range instructions {
		inst := &instructions[j]
		if err := validateInstruction("", inst); err != nil {
			return prependValidationPath(path+".instructions["+strconv.Itoa(j)+"]", err)
		}
		if err := validateInstructionRefs("", inst, refs, locals, upvalues, table); err != nil {
			return prependValidationPath(path+".instructions["+strconv.Itoa(j)+"]", err)
		}
		if inst.Op == OpReturn {
			instPath := path + ".instructions[" + strconv.Itoa(j) + "]"
			payload, err := validationPayload[ReturnPayload](instPath, inst)
			if err != nil {
				return err
			}
			if payload.ResultCount != len(fn.Signature.Results) {
				return schemaMismatchValidationError(instPath+".payload.result_count", fmt.Errorf("return result count mismatch: got %d, want %d", payload.ResultCount, len(fn.Signature.Results)))
			}
		}
		if inst.Op == OpTailCallDirect {
			instPath := path + ".instructions[" + strconv.Itoa(j) + "]"
			payload, err := validationPayload[CallPayload](instPath, inst)
			if err != nil {
				return err
			}
			if payload.ResultCount != len(fn.Signature.Results) {
				return schemaMismatchValidationError(instPath+".payload.result_count", fmt.Errorf("tail call result count mismatch: got %d, want %d", payload.ResultCount, len(fn.Signature.Results)))
			}
		}
		if inst.Op == OpLabel {
			instPath := path + ".instructions[" + strconv.Itoa(j) + "]"
			payload, err := validationPayload[LabelPayload](instPath, inst)
			if err != nil {
				return err
			}
			label := strings.TrimSpace(payload.Label)
			if label == "" {
				return missingValidationError(instPath+".payload.label", errors.New("missing label"))
			}
			if prev, ok := labels[label]; ok {
				return newCodedValidationError(ValidationLabelDuplicate, instPath+".payload.label", fmt.Errorf("duplicate label %q first seen at instruction %d", label, prev))
			}
			labels[label] = j
		}
	}
	for j := range instructions {
		inst := &instructions[j]
		switch inst.Op {
		case OpCompareBranch, OpTypeDispatch, OpJump, OpJumpIf:
		default:
			continue
		}
		instPath := path + ".instructions[" + strconv.Itoa(j) + "]"
		switch inst.Op {
		case OpCompareBranch:
			payload := inst.Payload.(CompareBranchPayload)
			if _, ok := labels[payload.Label]; !ok {
				return newCodedValidationError(ValidationLabelUnknown, instPath, fmt.Errorf("unknown label %q", payload.Label))
			}
		case OpTypeDispatch:
			payload, err := validationPayload[TypeDispatchPayload](instPath, inst)
			if err != nil {
				return err
			}
			if _, ok := labels[payload.Default]; !ok {
				return newCodedValidationError(ValidationLabelUnknown, instPath, fmt.Errorf("unknown label %q", payload.Default))
			}
			for _, match := range payload.Cases {
				if _, ok := labels[match.Label]; !ok {
					return newCodedValidationError(ValidationLabelUnknown, instPath, fmt.Errorf("unknown label %q", match.Label))
				}
			}
		case OpJump, OpJumpIf:
			payload, err := validationPayload[JumpPayload](instPath, inst)
			if err != nil {
				return err
			}
			label := strings.TrimSpace(payload.Label)
			if label == "" {
				return missingValidationError(instPath+".payload.label", errors.New("missing jump label"))
			}
			if _, ok := labels[label]; !ok {
				return newCodedValidationError(ValidationLabelUnknown, instPath+".payload.label", fmt.Errorf("unknown label %q", label))
			}
		}
	}
	for i, typ := range fn.Code.Types {
		if err := validateTypeRef(path+".code.types["+strconv.Itoa(i)+"]", typ, table); err != nil {
			return err
		}
	}
	if err := validateSlotCode(fn.Code, refs.constantOrder, fn.Locals, operations); err != nil {
		return newValidationError(path+".code", err)
	}
	if err := validateSlotTypes(fn, operations, refs, table); err != nil {
		return newValidationError(path+".code", err)
	}
	return nil
}

func validateResultLocals(path string, fn Function, locals map[string]types.TypeRef) error {
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
			return missingValidationError(path+"["+strconv.Itoa(i)+"]", errors.New("missing result local"))
		}
		if _, ok := locals[local]; !ok {
			return unknownValidationError(path+"["+strconv.Itoa(i)+"]", fmt.Errorf("unknown local slot %q", local))
		}
		if _, ok := seen[local]; ok {
			return newValidationError(path+"["+strconv.Itoa(i)+"]", fmt.Errorf("duplicate result local %q", local))
		}
		seen[local] = struct{}{}
	}
	return nil
}

func validateInstructionRefs(path string, inst *Instruction, refs artifactRefs, locals map[string]types.TypeRef, upvalues map[string]struct{}, table *types.TypeTable) error {
	switch inst.Op {
	case OpGetPath:
		payload := inst.Payload.(FieldPathPayload)
		if err := validateTypeRef(path+".type", payload.Type, table); err != nil {
			return err
		}
		_, err := fieldPathResult(table, payload)
		return err
	case OpCompareBranch:
		return validateTypeRef(path+".type", inst.Payload.(CompareBranchPayload).Type, table)
	case OpTypeDispatch:
		payload, err := validationPayload[TypeDispatchPayload](path, inst)
		if err != nil {
			return err
		}
		used := []string{payload.Subject}
		if payload.DefaultLocal != "" {
			used = append(used, payload.DefaultLocal)
		}
		for _, match := range payload.Cases {
			if match.Type.Valid() {
				if err := validateTypeRef(path+".payload.cases.type", match.Type, table); err != nil {
					return err
				}
			}
			if match.Binding != "" {
				used = append(used, match.Binding)
			}
		}
		for _, local := range used {
			if _, ok := locals[local]; !ok {
				return unknownValidationError(path, fmt.Errorf("unknown type dispatch local %q", local))
			}
		}
		subject := locals[payload.Subject]
		underlying := table.Underlying(subject)
		if underlying.Kind != types.Any && underlying.Kind != types.Interface && underlying != types.Builtin(types.PrimitiveError) {
			return newValidationError(path, errors.New("type dispatch subject must be an interface"))
		}
		relations := types.NewRelations(table)
		if payload.DefaultLocal != "" && !relations.Identical(locals[payload.DefaultLocal], subject).OK {
			return newValidationError(path, errors.New("type dispatch default binding must match subject type"))
		}
		for _, match := range payload.Cases {
			if match.Binding == "" {
				continue
			}
			wanted := match.Type
			if match.Original {
				wanted = subject
			}
			if !relations.Identical(locals[match.Binding], wanted).OK {
				return newValidationError(path, errors.New("type dispatch case binding has incompatible type"))
			}
		}
	case OpConst:
		payload, err := validationPayload[ConstPayload](path, inst)
		if err != nil {
			return err
		}
		if _, ok := refs.constants[payload.Constant]; !ok {
			return unknownValidationError(path+".payload.constant", fmt.Errorf("unknown constant id %q", payload.Constant))
		}
		if refs.untypedConstants[payload.Constant] {
			return newValidationError(path+".payload.constant", fmt.Errorf("untyped constant %q cannot be used by runtime instructions", payload.Constant))
		}
	case OpSelect:
		payload, err := validationPayload[SelectPayload](path, inst)
		if err != nil {
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
	case OpLoadLocal, OpStoreLocal, OpMapIterInit, OpMapIterNext, OpMapIterClose:
		payload, err := validationPayload[LocalPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.Rebind && inst.Op != OpStoreLocal {
			return newValidationError(path+".payload.rebind", errors.New("rebind is only valid for store_local"))
		}
		if _, ok := locals[payload.Local]; !ok {
			return unknownValidationError(path+".payload.local", fmt.Errorf("unknown local %q", payload.Local))
		}
	case OpLoadUpvalue, OpStoreUpvalue:
		payload, err := validationPayload[UpvaluePayload](path, inst)
		if err != nil {
			return err
		}
		if _, ok := upvalues[payload.Upvalue]; !ok {
			return unknownValidationError(path+".payload.upvalue", fmt.Errorf("unknown upvalue %q", payload.Upvalue))
		}
	case OpLoadGlobal, OpStoreGlobal:
		payload, err := validationPayload[GlobalPayload](path, inst)
		if err != nil {
			return err
		}
		if _, ok := refs.globals[payload.Global]; !ok {
			return unknownValidationError(path+".payload.global", fmt.Errorf("unknown global id %q", payload.Global))
		}
	case OpAddressOf:
		payload, err := validationPayload[AddressPayload](path, inst)
		if err != nil {
			return err
		}
		if err := validateAddressRef(path+".payload", payload, refs, locals, upvalues); err != nil {
			return err
		}
	case OpCallDirect, OpTailCallDirect:
		payload, err := validationPayload[CallPayload](path, inst)
		if err != nil {
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
	case OpCallInterface:
		payload, err := validationPayload[CallInterfacePayload](path, inst)
		if err != nil {
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
	case OpMakeClosure:
		payload, err := validationPayload[ClosurePayload](path, inst)
		if err != nil {
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
			if err := validateAddressRef(path+".payload.captures["+strconv.Itoa(i)+"]", capture, refs, locals, upvalues); err != nil {
				return err
			}
		}
	case OpCallFFI:
		payload, err := validationPayload[CallFFIPayload](path, inst)
		if err != nil {
			return err
		}
		if payload.ArgCount != 2 || payload.ResultCount != 3 {
			return schemaMismatchValidationError(path+".payload", errors.New("invalid FFI stack contract"))
		}
	case OpCallIntrinsic:
		payload, err := validationPayload[CallIntrinsicPayload](path, inst)
		if err != nil {
			return err
		}
		descriptor, ok := Intrinsic(payload.ID)
		if !ok || descriptor.ArgCount != payload.ArgCount || descriptor.ResultCount != payload.ResultCount {
			return newValidationError(path+".payload.id", fmt.Errorf("invalid intrinsic contract %q", payload.ID))
		}
	case OpLoadExport:
		payload, err := validationPayload[ExportPayload](path, inst)
		if err != nil {
			return err
		}
		exports, ok := refs.moduleExports[payload.ModulePath]
		if !ok {
			return unknownValidationError(path+".payload.module_path", fmt.Errorf("unknown module requirement %q", payload.ModulePath))
		}
		if _, ok := exports[payload.Export]; !ok {
			return unknownValidationError(path+".payload.export", fmt.Errorf("unknown module export %q", payload.Export))
		}
	case OpInitModule:
		payload, err := validationPayload[InitModulePayload](path, inst)
		if err != nil {
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

func validateAddressRef(path string, payload AddressPayload, refs artifactRefs, locals map[string]types.TypeRef, upvalues map[string]struct{}) error {
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
	return validateAddressPath(path+".path", payload.Path, locals)
}

func validateUniqueIDs(section string, n int, idAt func(int) string) error {
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		id := strings.TrimSpace(idAt(i))
		if id == "" {
			return missingValidationError(section+"["+strconv.Itoa(i)+"].id", errors.New("missing id"))
		}
		if prev, ok := seen[id]; ok {
			return newCodedValidationError(ValidationIDDuplicate, section+"["+strconv.Itoa(i)+"].id", fmt.Errorf("duplicate id %q first seen at %s[%d]", id, section, prev))
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
		if fn.Code != nil {
			out[fn.ID] = len(fn.Code.Instructions)
		}
	}
	return out
}
