package compiler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type functionRef struct {
	modulePath string
	function   string
}

type methodRef struct {
	functionRef
	signature string
	receiver  types.TypeKey
}

type reachabilityIndex struct {
	table          *types.TypeTable
	constants      map[string]types.TypeRef
	globals        map[string]types.TypeRef
	exports        map[string]ir.Export
	reachableTypes map[types.TypeRef]struct{}
	interfaceTypes map[types.TypeRef]struct{}
}

func retainReachableCode(ctx context.Context, source map[string]ir.Artifact, entries []ir.Entry) (map[string]ir.Artifact, error) {
	artifacts := make(map[string]ir.Artifact, len(source))
	functions := make(map[string]ir.Function)
	methods := make(map[string][]methodRef)
	methodsByReceiver := make(map[types.TypeKey][]functionRef)
	indexes := make(map[string]reachabilityIndex, len(source))
	queue := make([]functionRef, 0, len(source)+len(entries))
	keep := make(map[string]struct{})
	queued := make(map[string]struct{})
	enqueue := func(ref functionRef) {
		key := functionKey(ref.modulePath, ref.function)
		if _, retained := keep[key]; retained {
			return
		}
		if _, scheduled := queued[key]; scheduled {
			return
		}
		queued[key] = struct{}{}
		queue = append(queue, ref)
	}
	for modulePath, artifact := range source {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		artifact.TypeTable.Nodes = append([]types.TypeNode(nil), artifact.TypeTable.Nodes...)
		artifacts[modulePath] = artifact
		index := reachabilityIndex{
			table:          &artifact.TypeTable,
			reachableTypes: make(map[types.TypeRef]struct{}),
			interfaceTypes: make(map[types.TypeRef]struct{}),
			constants:      make(map[string]types.TypeRef, len(artifact.Constants)),
			globals:        make(map[string]types.TypeRef, len(artifact.Globals)),
			exports:        make(map[string]ir.Export, len(artifact.Exports)),
		}
		for _, constant := range artifact.Constants {
			index.constants[constant.ID] = constant.Type
		}
		for _, global := range artifact.Globals {
			index.globals[global.ID] = global.Type
		}
		for _, export := range artifact.Exports {
			index.exports[export.Name] = export
		}
		indexes[modulePath] = index
		for _, function := range artifact.Functions {
			functions[functionKey(modulePath, function.ID)] = function
		}
		if hasFunction(artifact, "fn.init") {
			enqueue(functionRef{modulePath, "fn.init"})
		}
		for _, typ := range index.table.DefinedNamed(artifact.Module.Path) {
			for _, method := range typ.Methods {
				owner := strings.TrimSpace(method.ModulePath)
				if owner == "" {
					owner = modulePath
				}
				if method.FunctionID != "" {
					ref := functionRef{owner, method.FunctionID}
					receiver, _ := namedReceiverType(index.table, method.Receiver, map[types.TypeRef]struct{}{})
					methods[method.Name] = append(methods[method.Name], methodRef{
						functionRef: ref,
						signature:   types.FormatSignature(index.table, method.Signature),
						receiver:    receiver,
					})
					if receiver.ModulePath != "" && receiver.DeclID != "" {
						methodsByReceiver[receiver] = append(methodsByReceiver[receiver], ref)
					}
				}
			}
		}
	}
	for _, entry := range entries {
		enqueue(functionRef{entry.ModulePath, entry.FunctionID})
	}
	reachableReceivers := make(map[types.TypeKey]struct{})
	interfaceRequirements := make(map[string]map[string]struct{})
	reflectMethodsEnabled := false
	var requireInterfaceMethod func(string, string)
	// Type tables stay immutable throughout the worklist. A module owns its
	// visited sets: anonymous node IDs are only unique within that table.
	// New method requirements still scan all previously reachable receivers.
	enqueueReachableType := func(table *types.TypeTable, seen map[types.TypeRef]struct{}, ref types.TypeRef) {
		var visit func(types.TypeRef)
		visit = func(current types.TypeRef) {
			if !current.Valid() {
				return
			}
			if _, ok := seen[current]; ok {
				return
			}
			seen[current] = struct{}{}
			if current.Kind == types.Primitive && current.Primitive == types.PrimitiveError {
				requireInterfaceMethod("Error", types.FormatSignature(table, types.FunctionSignature{
					Results: []types.TypeRef{types.Builtin(types.PrimitiveString)},
				}))
			}
			if iface, ok := table.IsInterface(current); ok && len(iface.Methods) == 1 {
				method := iface.Methods[0]
				if method.Name == "Error" && len(method.Signature.Params) == 0 && len(method.Signature.Results) == 1 &&
					types.NewRelations(table).Identical(method.Signature.Results[0], types.Builtin(types.PrimitiveString)).OK {
					requireInterfaceMethod("Error", types.FormatSignature(table, method.Signature))
				}
			}
			if key, ok := namedReceiverType(table, current, map[types.TypeRef]struct{}{}); ok {
				if _, known := reachableReceivers[key]; !known {
					reachableReceivers[key] = struct{}{}
					if reflectMethodsEnabled {
						for _, method := range methodsByReceiver[key] {
							enqueue(method)
						}
					}
					for name, signatures := range interfaceRequirements {
						for _, candidate := range methods[name] {
							if candidate.receiver == key {
								if _, required := signatures[candidate.signature]; required {
									enqueue(candidate.functionRef)
								}
							}
						}
					}
				}
			}
			node, ok := table.Node(current)
			if !ok && current.Kind == types.Named {
				node, ok = table.Named(current.Named)
			}
			if !ok {
				return
			}
			visit(node.Underlying)
			visit(node.AliasTarget)
			visit(node.Elem)
			visit(node.Key)
			visit(node.Constraint)
			visit(node.Base)
			if node.Signature != nil {
				for _, param := range node.Signature.Params {
					visit(param.Type)
				}
				for _, result := range node.Signature.Results {
					visit(result)
				}
			}
			for _, item := range node.Tuple {
				visit(item)
			}
			for _, field := range node.Fields {
				visit(field.Type)
			}
			for _, term := range node.Terms {
				visit(term.Type)
			}
			for _, argument := range node.TypeArgs {
				visit(argument)
			}
		}
		visit(ref)
	}
	enableReflectMethods := func() {
		if reflectMethodsEnabled {
			return
		}
		reflectMethodsEnabled = true
		for receiver := range reachableReceivers {
			for _, method := range methodsByReceiver[receiver] {
				enqueue(method)
			}
		}
	}
	requireInterfaceMethod = func(name, signature string) {
		signatures := interfaceRequirements[name]
		if signatures == nil {
			signatures = make(map[string]struct{})
			interfaceRequirements[name] = signatures
		}
		signatures[signature] = struct{}{}
		for _, candidate := range methods[name] {
			if candidate.signature != signature {
				continue
			}
			if _, reachable := reachableReceivers[candidate.receiver]; reachable {
				enqueue(candidate.functionRef)
			}
		}
	}
	enqueueInterfaceMethods := func(table *types.TypeTable, seen map[types.TypeRef]struct{}, root types.TypeRef) {
		var visit func(types.TypeRef)
		visit = func(ref types.TypeRef) {
			if !ref.Valid() {
				return
			}
			if _, ok := seen[ref]; ok {
				return
			}
			seen[ref] = struct{}{}
			if ref.Kind == types.Primitive && ref.Primitive == types.PrimitiveError {
				requireInterfaceMethod("Error", types.FormatSignature(table, types.FunctionSignature{
					Results: []types.TypeRef{types.Builtin(types.PrimitiveString)},
				}))
			}
			if iface, ok := table.IsInterface(ref); ok {
				for _, method := range iface.Methods {
					signature := types.FormatSignature(table, method.Signature)
					requireInterfaceMethod(method.Name, signature)
					for _, param := range method.Signature.Params {
						visit(param.Type)
					}
					for _, result := range method.Signature.Results {
						visit(result)
					}
				}
			}
			underlying := table.Underlying(ref)
			node, ok := table.Node(underlying)
			if !ok {
				return
			}
			visit(node.Elem)
			visit(node.Key)
			visit(node.Underlying)
			visit(node.AliasTarget)
			visit(node.Constraint)
			visit(node.Base)
			if node.Signature != nil {
				for _, param := range node.Signature.Params {
					visit(param.Type)
				}
				for _, result := range node.Signature.Results {
					visit(result)
				}
			}
			for _, item := range node.Tuple {
				visit(item)
			}
			for _, field := range node.Fields {
				visit(field.Type)
			}
			for _, term := range node.Terms {
				visit(term.Type)
			}
			for _, argument := range node.TypeArgs {
				visit(argument)
			}
		}
		visit(root)
	}
	for next := 0; next < len(queue); next++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref := queue[next]
		key := functionKey(ref.modulePath, ref.function)
		delete(queued, key)
		if _, ok := keep[key]; ok {
			continue
		}
		function, ok := functions[key]
		if !ok {
			return nil, fmt.Errorf("linked function %s.%s is missing", ref.modulePath, ref.function)
		}
		keep[key] = struct{}{}
		artifact := source[ref.modulePath]
		index := indexes[ref.modulePath]
		for _, param := range function.Signature.Params {
			enqueueReachableType(index.table, index.reachableTypes, param.Type)
		}
		for _, result := range function.Signature.Results {
			enqueueReachableType(index.table, index.reachableTypes, result)
		}
		for _, local := range function.Locals {
			enqueueReachableType(index.table, index.reachableTypes, local.Type)
		}
		for _, upvalue := range function.Upvalues {
			enqueueReachableType(index.table, index.reachableTypes, upvalue.Type)
		}
		if function.Code != nil {
			for _, typ := range function.Code.Types {
				enqueueReachableType(index.table, index.reachableTypes, typ)
			}
			for _, operands := range function.Code.Operands {
				for _, input := range operands.Inputs {
					if input.Kind == ir.OperandConstant {
						if uint64(input.Index) >= uint64(len(artifact.Constants)) {
							return nil, fmt.Errorf("%s.%s constant operand out of range", ref.modulePath, ref.function)
						}
						enqueueReachableType(index.table, index.reachableTypes, artifact.Constants[input.Index].Type)
					}
				}
			}
		}
		operations, err := function.Operations()
		if err != nil {
			return nil, err
		}
		for _, instruction := range operations {
			switch instruction.Op {
			case ir.OpCompareBranch:
				enqueueReachableType(index.table, index.reachableTypes, instruction.Payload.(ir.CompareBranchPayload).Type)
			case ir.OpTypeDispatch:
				for _, match := range instruction.Payload.(ir.TypeDispatchPayload).Cases {
					if match.Type.Valid() {
						enqueueReachableType(index.table, index.reachableTypes, match.Type)
					}
				}
			case ir.OpGetPath:
				enqueueReachableType(index.table, index.reachableTypes, instruction.Payload.(ir.FieldPathPayload).Type)
			case ir.OpZero, ir.OpTypeAssert, ir.OpTypeAssertOK, ir.OpConvert:
				var payload ir.TypePayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode type payload in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.Type)
			case ir.OpMakeSequence:
				var payload ir.MakeSequencePayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode make_sequence in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.Type)
			case ir.OpMakeMap:
				var payload ir.MakeMapPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode make_map in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.Type)
			case ir.OpMakeStruct:
				var payload ir.MakeStructPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode make_struct in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.Type)
			case ir.OpMakeSlice:
				var payload ir.MakeSlicePayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode make_slice in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.Type)
			case ir.OpMakeWaitable:
				var payload ir.MakeWaitablePayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode make_waitable in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.Type)
			case ir.OpCallDirect, ir.OpTailCallDirect:
				var payload ir.CallPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode %s in %s.%s: %w", instruction.Op, ref.modulePath, ref.function, err)
				}
				modulePath := strings.TrimSpace(payload.ModulePath)
				if modulePath == "" {
					modulePath = ref.modulePath
				}
				enqueue(functionRef{modulePath, payload.Function})
				if modulePath == "reflect" && ref.modulePath != "reflect" {
					switch payload.Function {
					case "method.Value.NumMethod", "method.Value.Method", "method.Value.MethodByName", "method.Value.Methods":
						enableReflectMethods()
					}
				}
			case ir.OpCallIntrinsic:
				var payload ir.CallIntrinsicPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode call_intrinsic in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				descriptor, ok := ir.Intrinsic(payload.ID)
				if !ok || descriptor.DynamicResult.ModulePath == "" || descriptor.DynamicResult.DeclID == "" {
					continue
				}
				dependency, ok := indexes[descriptor.DynamicResult.ModulePath]
				if !ok {
					return nil, fmt.Errorf("intrinsic %s requires unavailable dynamic result type %s.%s", payload.ID, descriptor.DynamicResult.ModulePath, descriptor.DynamicResult.DeclID)
				}
				node, ok := dependency.table.Named(descriptor.DynamicResult)
				if !ok {
					return nil, fmt.Errorf("intrinsic %s dynamic result type %s.%s is missing", payload.ID, descriptor.DynamicResult.ModulePath, descriptor.DynamicResult.DeclID)
				}
				enqueueReachableType(dependency.table, dependency.reachableTypes, types.TypeRef{Kind: types.Named, Named: node.Identity, Node: node.ID})
			case ir.OpMakeClosure:
				var payload ir.ClosurePayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode make_closure in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				modulePath := strings.TrimSpace(payload.ModulePath)
				if modulePath == "" {
					modulePath = ref.modulePath
				}
				enqueue(functionRef{modulePath, payload.Function})
			case ir.OpLoadExport:
				var payload ir.ExportPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode module_member in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				if dependency, ok := indexes[payload.ModulePath]; ok {
					export, found := indexes[payload.ModulePath].exports[payload.Export]
					if !found {
						return nil, fmt.Errorf("%s.%s references missing export %s.%s", ref.modulePath, ref.function, payload.ModulePath, payload.Export)
					}
					enqueueReachableType(dependency.table, dependency.reachableTypes, export.Type)
					if export.Kind == "function" {
						enqueue(functionRef{payload.ModulePath, export.ID})
					}
				} else {
					return nil, fmt.Errorf("%s.%s references missing module %s", ref.modulePath, ref.function, payload.ModulePath)
				}
			case ir.OpCallInterface:
				var payload ir.CallInterfacePayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode call_interface in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				enqueueReachableType(index.table, index.reachableTypes, payload.InterfaceType)
				enqueueInterfaceMethods(index.table, index.interfaceTypes, payload.InterfaceType)
				if key, ok := namedReceiverType(index.table, payload.InterfaceType, map[types.TypeRef]struct{}{}); ok && ref.modulePath != "reflect" && key.ModulePath == "reflect" && key.DeclID == "Type" {
					switch payload.Method {
					case "NumMethod", "Method", "MethodByName", "Methods":
						enableReflectMethods()
					}
				}
			case ir.OpConst:
				var payload ir.ConstPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode const in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				if typ, ok := index.constants[payload.Constant]; ok {
					enqueueReachableType(index.table, index.reachableTypes, typ)
				}
			case ir.OpLoadGlobal:
				var payload ir.GlobalPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode load_global in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				if typ, ok := index.globals[payload.Global]; ok {
					enqueueReachableType(index.table, index.reachableTypes, typ)
				}
			case ir.OpAddressOf:
				var payload ir.AddressPayload
				if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
					return nil, fmt.Errorf("decode address_of in %s.%s: %w", ref.modulePath, ref.function, err)
				}
				switch payload.Kind {
				case "global":
					if typ, ok := index.globals[payload.Global]; ok {
						enqueueReachableType(index.table, index.reachableTypes, typ)
					}
				case "export":
					if dependency, ok := indexes[payload.ModulePath]; ok {
						export, found := indexes[payload.ModulePath].exports[payload.Export]
						if !found || export.Kind != "global" {
							return nil, fmt.Errorf("address target %s.%s is not an exported variable", payload.ModulePath, payload.Export)
						}
						enqueueReachableType(dependency.table, dependency.reachableTypes, export.Type)
					}
				}
			}
		}
	}
	for modulePath, artifact := range artifacts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		retained := make([]ir.Function, 0, len(artifact.Functions))
		for _, function := range artifact.Functions {
			if _, ok := keep[functionKey(modulePath, function.ID)]; ok {
				retained = append(retained, function)
			}
		}
		artifact.Functions = retained
		exports := make([]ir.Export, 0, len(artifact.Exports))
		for _, export := range artifact.Exports {
			if export.Kind != "function" {
				exports = append(exports, export)
				continue
			}
			if _, ok := keep[functionKey(modulePath, export.ID)]; ok {
				exports = append(exports, export)
			}
		}
		artifact.Exports = exports
		for i := range artifact.TypeTable.Nodes {
			node := &artifact.TypeTable.Nodes[i]
			if node.Kind != types.Named {
				continue
			}
			methods := make([]types.Method, 0, len(node.Methods))
			for _, method := range node.Methods {
				owner := strings.TrimSpace(method.ModulePath)
				if owner == "" {
					owner = modulePath
				}
				if method.FunctionID == "" {
					methods = append(methods, method)
					continue
				}
				if _, ok := keep[functionKey(owner, method.FunctionID)]; ok {
					methods = append(methods, method)
					continue
				}
				method.FunctionID = ""
				methods = append(methods, method)
			}
			node.Methods = methods
		}
		if err := artifact.TypeTable.Reindex(); err != nil {
			return nil, fmt.Errorf("reindex retained types for %s: %w", modulePath, err)
		}
		requirements := make([]ir.Requirement, 0, len(artifact.Requirements))
		for _, requirement := range artifact.Requirements {
			switch requirement.Kind {
			case ir.RequirementSource:
				if dependency, ok := source[requirement.ModulePath]; ok {
					exports := make([]string, 0, len(requirement.Exports))
					for _, name := range requirement.Exports {
						retain := true
						for _, exported := range dependency.Exports {
							if exported.Name == name && exported.Kind == "function" {
								_, retain = keep[functionKey(requirement.ModulePath, exported.ID)]
								break
							}
						}
						if retain {
							exports = append(exports, name)
						}
					}
					requirement.Exports = exports
				}
				requirements = append(requirements, requirement)
				continue
			default:
				return nil, fmt.Errorf("unsupported requirement kind %q", requirement.Kind)
			}
		}
		artifact.Requirements = requirements
		artifacts[modulePath] = artifact
	}
	return artifacts, nil
}

func namedReceiverType(table *types.TypeTable, ref types.TypeRef, seen map[types.TypeRef]struct{}) (types.TypeKey, bool) {
	if !ref.Valid() {
		return types.TypeKey{}, false
	}
	if _, ok := seen[ref]; ok {
		return types.TypeKey{}, false
	}
	seen[ref] = struct{}{}
	if ref.Kind == types.Named && ref.Named.ModulePath != "" && ref.Named.DeclID != "" {
		return ref.Named, true
	}
	if table == nil {
		return types.TypeKey{}, false
	}
	node, ok := table.Node(ref)
	if !ok {
		return types.TypeKey{}, false
	}
	if node.Identity.ModulePath != "" && node.Identity.DeclID != "" {
		return node.Identity, true
	}
	if node.Kind == types.Pointer || node.Kind == types.Instance {
		candidate := node.Elem
		if node.Kind == types.Instance {
			candidate = node.Base
		}
		return namedReceiverType(table, candidate, seen)
	}
	return types.TypeKey{}, false
}

func functionKey(modulePath, function string) string {
	return modulePath + "::" + function
}

func hasFunction(artifact ir.Artifact, id string) bool {
	for _, function := range artifact.Functions {
		if function.ID == id {
			return true
		}
	}
	return false
}

func dependencyOrder(preferred []string, artifacts map[string]ir.Artifact) []string {
	seen := make(map[string]bool, len(artifacts))
	order := make([]string, 0, len(artifacts))
	var visit func(string)
	visit = func(modulePath string) {
		if seen[modulePath] {
			return
		}
		seen[modulePath] = true
		artifact, ok := artifacts[modulePath]
		if !ok {
			return
		}
		dependencies := make([]string, 0, len(artifact.Requirements))
		for _, requirement := range artifact.Requirements {
			if requirement.Kind == ir.RequirementSource {
				dependencies = append(dependencies, requirement.ModulePath)
			}
		}
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			visit(dependency)
		}
		order = append(order, modulePath)
	}
	for _, modulePath := range preferred {
		visit(modulePath)
	}
	rest := make([]string, 0, len(artifacts))
	for modulePath := range artifacts {
		if !seen[modulePath] {
			rest = append(rest, modulePath)
		}
	}
	sort.Strings(rest)
	for _, modulePath := range rest {
		visit(modulePath)
	}
	return order
}
