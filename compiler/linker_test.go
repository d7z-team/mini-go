package compiler

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPrepareRetainsEntryInitAndCallClosure(t *testing.T) {
	artifact := ir.NewArtifact("example/main", "main")
	artifact.TypeTable = *types.NewTable()
	artifact.Functions = []ir.Function{
		{ID: "fn.init", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
		{ID: "fn.main", Code: linkerTestCode(nil, []ir.Instruction{
			{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.used"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
		}, []ir.SlotOperands{{}, {}})},
		{ID: "fn.used", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
		{ID: "fn.unused", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
	}
	unreachable := ir.NewArtifact("example/unreachable", "unreachable")
	unreachable.TypeTable = *types.NewTable()
	artifacts := map[string]ir.Artifact{"example/main": artifact, "example/unreachable": unreachable}
	image, err := linkExecutionImage(linkRequest{
		CompilerID: "compiler", ContractID: ir.ExecutionContract, Root: "example/main",
		Entries:   []entrySelection{{Name: ir.DefaultEntryName, ModulePath: "example/main", Function: "main"}},
		Artifacts: artifacts, Symbols: linkerSymbols(artifacts),
		Order: []string{"example/unreachable", "example/main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := ir.DecodeJSON(image.Packages["example/main"].Artifact)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"fn.init": true, "fn.main": true, "fn.used": true}
	if len(linked.Functions) != len(want) {
		t.Fatalf("retained functions = %#v", linked.Functions)
	}
	for _, function := range linked.Functions {
		if !want[function.ID] {
			t.Fatalf("unexpected retained function %q", function.ID)
		}
	}
	if _, ok := image.Packages["example/unreachable"]; ok {
		t.Fatalf("unreachable package entered execution image: %#v", image.Packages)
	}
}

func TestReachabilityRejectsMissingInstructionDescriptor(t *testing.T) {
	artifact := ir.NewArtifact("example/main", "main")
	artifact.TypeTable = *types.NewTable()
	artifact.Functions = []ir.Function{
		{ID: "fn.main", Code: &ir.SlotCode{Instructions: []ir.SlotInstruction{{Op: ir.OpCallDirect}}, Descriptors: ir.DescriptorTables{Local: []ir.LocalPayload{{Local: "fn.used"}}}, Operands: []ir.SlotOperands{{}}}},
		{Code: &ir.SlotCode{}, ID: "fn.used"},
	}
	_, err := retainReachableCode(context.Background(),
		map[string]ir.Artifact{"example/main": artifact},
		[]ir.Entry{{Name: "default", ModulePath: "example/main", FunctionID: "fn.main"}},
	)
	if err == nil || !strings.Contains(err.Error(), "descriptor") {
		t.Fatalf("reachability error = %v, want missing call descriptor", err)
	}
}

func TestLinkRejectsMissingDependencyMembersBeforePruning(t *testing.T) {
	for _, kind := range []string{"requirement", "call", "closure", "global"} {
		t.Run(kind, func(t *testing.T) {
			dependency := ir.NewArtifact("example/lib", "lib")
			root := ir.NewArtifact("example/main", "main")
			root.Requirements = []ir.Requirement{{Kind: ir.RequirementSource, ModulePath: "example/lib"}}
			instructions := []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}
			operands := []ir.SlotOperands{{}}
			var slotTypes []types.TypeRef
			switch kind {
			case "requirement":
				root.Requirements[0].Exports = []string{"Missing"}
			case "call":
				operands = append(operands, ir.SlotOperands{})
				instructions = append([]ir.Instruction{{Op: ir.OpCallDirect, Payload: ir.CallPayload{ModulePath: "example/lib", Function: "fn.Missing"}}}, instructions...)
			case "closure":
				ref := types.TypeRef{Kind: types.Function, Node: "fixture.function"}
				err := root.TypeTable.Add(types.TypeNode{ID: ref.Node, Kind: types.Function, Signature: &types.FunctionSignature{}})
				if err != nil {
					t.Fatal(err)
				}
				slotTypes = []types.TypeRef{ref}
				operands = []ir.SlotOperands{{Outputs: []uint32{0}}, {Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}}, {}}
				instructions = append([]ir.Instruction{{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{ModulePath: "example/lib", Function: "fn.Missing"}}, {Op: ir.OpPop}}, instructions...)
			case "global":
				ref := types.TypeRef{Kind: types.Pointer, Node: "fixture.pointer"}
				err := root.TypeTable.Add(types.TypeNode{ID: ref.Node, Kind: types.Pointer, Elem: types.Builtin(types.PrimitiveInt)})
				if err != nil {
					t.Fatal(err)
				}
				slotTypes = []types.TypeRef{ref}
				operands = []ir.SlotOperands{{Outputs: []uint32{0}}, {Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}}, {}}
				root.Requirements[0].Exports = []string{"Missing"}
				instructions = append([]ir.Instruction{{Op: ir.OpAddressOf, Payload: ir.AddressPayload{Kind: "export", ModulePath: "example/lib", Export: "Missing"}}, {Op: ir.OpPop}}, instructions...)
			}
			root.Functions = []ir.Function{
				{ID: "fn.main", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
				{ID: "fn.unused", Code: linkerTestCode(slotTypes, instructions, operands)},
			}
			artifacts := map[string]ir.Artifact{"example/main": root, "example/lib": dependency}
			_, err := linkExecutionImage(linkRequest{CompilerID: "compiler", ContractID: ir.ExecutionContract, Root: "example/main", Entries: []entrySelection{{Name: ir.DefaultEntryName, ModulePath: "example/main", Function: "main"}}, Artifacts: artifacts, Symbols: linkerSymbols(artifacts)})
			if err == nil || !strings.Contains(err.Error(), "Missing") {
				t.Fatalf("link error: %v", err)
			}
		})
	}
}

func TestPrepareDoesNotMutateInputArtifacts(t *testing.T) {
	artifact := ir.NewArtifact("example/main", "main")
	receiver := types.TypeRef{
		Kind:  types.Named,
		Named: types.TypeKey{ModulePath: "example/main", DeclID: "Value"},
		Node:  "named.value",
	}
	artifact.TypeTable = *types.NewTable(types.TypeNode{
		ID:         "named.value",
		Kind:       types.Named,
		Identity:   types.TypeKey{ModulePath: "example/main", DeclID: "Value"},
		Underlying: types.AnyType(),
		Methods: []types.Method{
			{Name: "First", Receiver: receiver, FunctionID: "fn.first"},
			{Name: "Second", Receiver: receiver, FunctionID: "fn.second"},
		},
	})
	artifact.Functions = []ir.Function{
		{ID: "fn.first", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
		{ID: "fn.second", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
	}
	artifact.Exports = []ir.Export{{Name: "First", Kind: "function", ID: "fn.first"}, {Name: "Second", Kind: "function", ID: "fn.second"}}
	artifacts := map[string]ir.Artifact{"example/main": artifact}
	request := linkRequest{
		CompilerID: "compiler", ContractID: ir.ExecutionContract, Root: "example/main",
		Entries:   []entrySelection{{Name: ir.DefaultEntryName, ModulePath: "example/main", Function: "first"}},
		Artifacts: artifacts, Symbols: linkerSymbols(artifacts), Order: []string{"example/main"},
	}
	if _, err := linkExecutionImage(request); err != nil {
		t.Fatal(err)
	}
	unchanged := artifacts["example/main"]
	if len(unchanged.Functions) != 2 || unchanged.Functions[0].ID != "fn.first" || unchanged.Functions[1].ID != "fn.second" || len(unchanged.Exports) != 2 || len(unchanged.TypeTable.Nodes[0].Methods) != 2 {
		t.Fatalf("Prepare mutated input artifact: %#v", unchanged)
	}
	request.Entries[0].Function = "second"
	if _, err := linkExecutionImage(request); err != nil {
		t.Fatalf("second Prepare using the same artifact failed: %v", err)
	}
}

func TestPreparePrunesUnreachableDependencyFunctions(t *testing.T) {
	main := ir.NewArtifact("example/main", "main")
	main.TypeTable = *types.NewTable()
	main.Functions = []ir.Function{{ID: "fn.main", Code: linkerTestCode(nil, []ir.Instruction{
		{Op: ir.OpCallDirect, Payload: ir.CallPayload{ModulePath: "example/lib", Function: "fn.used"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
	}, []ir.SlotOperands{{}, {}})}}
	main.Requirements = []ir.Requirement{{Kind: "source", ModulePath: "example/lib", Exports: []string{"Unused", "Used"}}}
	lib := ir.NewArtifact("example/lib", "lib")
	lib.TypeTable = *types.NewTable()
	lib.Functions = []ir.Function{
		{ID: "fn.used", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
		{ID: "fn.unused", Code: linkerTestCode(nil, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, []ir.SlotOperands{{}})},
	}
	lib.Exports = []ir.Export{
		{Name: "Unused", Kind: "function", ID: "fn.unused"},
		{Name: "Used", Kind: "function", ID: "fn.used"},
	}
	artifacts := map[string]ir.Artifact{"example/main": main, "example/lib": lib}
	image, err := linkExecutionImage(linkRequest{
		CompilerID: "compiler", ContractID: ir.ExecutionContract, Root: "example/main",
		Entries:   []entrySelection{{Name: ir.DefaultEntryName, ModulePath: "example/main", Function: "main"}},
		Artifacts: artifacts, Symbols: linkerSymbols(artifacts),
		Order: []string{"example/lib", "example/main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	linkedMain, err := ir.DecodeJSON(image.Packages["example/main"].Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(linkedMain.Requirements) != 1 || len(linkedMain.Requirements[0].Exports) != 1 || linkedMain.Requirements[0].Exports[0] != "Used" {
		t.Fatalf("linked source requirement = %#v", linkedMain.Requirements)
	}
	linkedLib, err := ir.DecodeJSON(image.Packages["example/lib"].Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(linkedLib.Functions) != 1 || linkedLib.Functions[0].ID != "fn.used" {
		t.Fatalf("linked dependency functions = %#v", linkedLib.Functions)
	}
}

func linkerSymbols(artifacts map[string]ir.Artifact) map[string]ir.PackageSymbols {
	symbols := make(map[string]ir.PackageSymbols, len(artifacts))
	for modulePath, artifact := range artifacts {
		pkg := ir.PackageSymbols{ModulePath: modulePath}
		for _, function := range artifact.Functions {
			name := strings.TrimPrefix(function.ID, "fn.")
			pkg.Functions = append(pkg.Functions, ir.FunctionSymbols{ID: function.ID, Name: name})
		}
		symbols[modulePath] = pkg
	}
	return symbols
}

func TestPrepareRetainsInterfaceMethodsForReachableImplementations(t *testing.T) {
	methodSignature := types.FunctionSignature{Results: []types.TypeRef{types.Builtin(types.PrimitiveString)}}
	key := types.TypeKey{ModulePath: "example/main", DeclID: "Reader"}
	interfaceRef := types.TypeRef{Kind: types.Named, Named: key, Node: "named.reader"}
	valueKey := types.TypeKey{ModulePath: "example/main", DeclID: "value"}
	valueRef := types.TypeRef{Kind: types.Named, Named: valueKey, Node: "named.value"}
	containerKey := types.TypeKey{ModulePath: "example/main", DeclID: "container"}
	table := types.NewTable(
		types.TypeNode{ID: "named.reader", Kind: types.Named, Identity: key, Underlying: types.TypeRef{Kind: types.Interface, Node: "interface.reader"}},
		types.TypeNode{ID: "interface.reader", Kind: types.Interface, Methods: []types.Method{{Name: "Name", Signature: methodSignature}, {Name: "Unused", Signature: methodSignature}}},
		types.TypeNode{ID: "slice.reader", Kind: types.Slice, Elem: interfaceRef},
		types.TypeNode{ID: "named.container", Kind: types.Named, Identity: containerKey, Underlying: types.TypeRef{Kind: types.Struct, Node: "struct.container"}},
		types.TypeNode{ID: "struct.container", Kind: types.Struct, Fields: []types.Field{{Name: "value", Type: types.TypeRef{Kind: types.Named, Named: valueKey}}}},
		types.TypeNode{ID: "ptr.container", Kind: types.Pointer, Elem: types.TypeRef{Kind: types.Named, Named: containerKey}},
		types.TypeNode{ID: "named.value", Kind: types.Named, Identity: valueKey, Underlying: types.AnyType(), Methods: []types.Method{
			{Name: "Name", Signature: methodSignature, FunctionID: "method.value.Name", Receiver: valueRef},
			{Name: "Unused", Signature: methodSignature, FunctionID: "method.value.Unused", Receiver: valueRef},
		}},
	)
	artifact := ir.NewArtifact("example/main", "main")
	artifact.TypeTable = *table
	artifact.Functions = []ir.Function{
		{ID: "fn.main", Locals: []ir.Local{
			{ID: "local.values", Type: types.TypeRef{Kind: types.Slice, Node: "slice.reader"}},
			{ID: "local.container", Type: types.TypeRef{Kind: types.Pointer, Node: "ptr.container"}},
		}, Code: linkerTestCode([]types.TypeRef{types.Builtin(types.PrimitiveString)}, []ir.Instruction{{
			Op: ir.OpCallInterface,
			Payload: ir.CallInterfacePayload{
				InterfaceType: interfaceRef, Method: "Name", ResultCount: 1,
			},
		}}, []ir.SlotOperands{{Inputs: []ir.Operand{{Kind: ir.OperandLocal}}, Outputs: []uint32{0}}})},
		{Code: &ir.SlotCode{}, ID: "method.value.Name", Signature: methodSignature},
		{Code: &ir.SlotCode{}, ID: "method.value.Unused", Signature: methodSignature},
	}
	linked, err := retainReachableCode(context.Background(), map[string]ir.Artifact{"example/main": artifact}, []ir.Entry{{Name: "default", ModulePath: "example/main", FunctionID: "fn.main"}})
	if err != nil {
		t.Fatal(err)
	}
	functions := linked["example/main"].Functions
	if len(functions) != 3 || functions[0].ID != "fn.main" || functions[1].ID != "method.value.Name" || functions[2].ID != "method.value.Unused" {
		t.Fatalf("retained functions = %#v", functions)
	}
	linkedArtifact := linked["example/main"]
	valueType, ok := linkedArtifact.TypeTable.Named(valueKey)
	if !ok || len(valueType.Methods) != 2 || valueType.Methods[0].FunctionID != "method.value.Name" || valueType.Methods[1].FunctionID != "method.value.Unused" {
		t.Fatalf("retained method metadata = %#v", valueType.Methods)
	}
}

func TestReachableTypeTraversalKeepsModuleOwnershipAndLateRequirements(t *testing.T) {
	methodSignature := types.FunctionSignature{Results: []types.TypeRef{types.Builtin(types.PrimitiveString)}}
	payload := types.TypeRef{Kind: types.Struct, Node: "shared.payload"}
	reader := types.TypeRef{Kind: types.Interface, Node: "interface.reader"}
	valueKey := types.TypeKey{ModulePath: "example/b", DeclID: "value"}
	value := types.TypeRef{Kind: types.Named, Named: valueKey, Node: "named.value"}
	a := ir.NewArtifact("example/a", "a")
	a.TypeTable = *types.NewTable(types.TypeNode{ID: payload.Node, Kind: types.Struct})
	a.Functions = []ir.Function{{ID: "fn.main", Locals: []ir.Local{{ID: "payload", Type: payload}}, Code: &ir.SlotCode{}}}
	b := ir.NewArtifact("example/b", "b")
	b.TypeTable = *types.NewTable(
		types.TypeNode{ID: payload.Node, Kind: types.Struct, Fields: []types.Field{{Name: "value", Type: value}}},
		types.TypeNode{ID: reader.Node, Kind: types.Interface, Methods: []types.Method{{Name: "Name", Signature: methodSignature}}},
		types.TypeNode{ID: value.Node, Kind: types.Named, Identity: valueKey, Underlying: types.AnyType(), Methods: []types.Method{{Name: "Name", Signature: methodSignature, FunctionID: "method.value.Name", Receiver: value}}},
	)
	b.Functions = []ir.Function{
		{ID: "fn.main", Locals: []ir.Local{{ID: "payload", Type: payload}}, Code: linkerTestCode(nil,
			[]ir.Instruction{{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.late"}}}, []ir.SlotOperands{{}})},
		{ID: "fn.late", Locals: []ir.Local{{ID: "reader", Type: reader}}, Code: linkerTestCode([]types.TypeRef{types.Builtin(types.PrimitiveString)},
			[]ir.Instruction{{Op: ir.OpCallInterface, Payload: ir.CallInterfacePayload{InterfaceType: reader, Method: "Name", ResultCount: 1}}},
			[]ir.SlotOperands{{Inputs: []ir.Operand{{Kind: ir.OperandLocal}}, Outputs: []uint32{0}}})},
		{ID: "method.value.Name", Signature: methodSignature, Code: &ir.SlotCode{}},
	}
	linked, err := retainReachableCode(context.Background(), map[string]ir.Artifact{"example/a": a, "example/b": b}, []ir.Entry{
		{Name: "a", ModulePath: "example/a", FunctionID: "fn.main"},
		{Name: "b", ModulePath: "example/b", FunctionID: "fn.main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasFunction(linked["example/b"], "method.value.Name") {
		t.Fatal("late interface requirement lost the receiver reached through module b's anonymous type")
	}
}

func TestPrepareKeepsInterfaceDispatchWithinReachableTypes(t *testing.T) {
	methodSignature := types.FunctionSignature{Results: []types.TypeRef{types.Builtin(types.PrimitiveString)}}
	interfaceKey := types.TypeKey{ModulePath: "example/main", DeclID: "Reader"}
	table := types.NewTable(
		types.TypeNode{ID: "named.reader", Kind: types.Named, Identity: interfaceKey, Underlying: types.TypeRef{Kind: types.Interface, Node: "interface.reader"}},
		types.TypeNode{ID: "interface.reader", Kind: types.Interface, Methods: []types.Method{{Name: "Name", Signature: methodSignature}}},
		types.TypeNode{ID: "named.value", Kind: types.Named, Identity: types.TypeKey{ModulePath: "example/main", DeclID: "value"}, Underlying: types.AnyType(), Methods: []types.Method{{Name: "Name", Signature: methodSignature, FunctionID: "method.value.Name", Receiver: types.TypeRef{Kind: types.Named, Named: types.TypeKey{ModulePath: "example/main", DeclID: "value"}, Node: "named.value"}}}},
	)
	artifact := ir.NewArtifact("example/main", "main")
	artifact.TypeTable = *table
	artifact.Functions = []ir.Function{
		{Code: &ir.SlotCode{}, ID: "fn.main", Locals: []ir.Local{
			{ID: "local.reader", Type: types.TypeRef{Kind: types.Named, Named: interfaceKey, Node: "named.reader"}},
			{ID: "local.value", Type: types.TypeRef{Kind: types.Named, Named: types.TypeKey{ModulePath: "example/main", DeclID: "value"}, Node: "named.value"}},
		}},
		{Code: &ir.SlotCode{}, ID: "method.value.Name", Signature: methodSignature},
	}
	linked, err := retainReachableCode(context.Background(), map[string]ir.Artifact{"example/main": artifact}, []ir.Entry{{Name: "default", ModulePath: "example/main", FunctionID: "fn.main"}})
	if err != nil {
		t.Fatal(err)
	}
	functions := linked["example/main"].Functions
	if len(functions) != 1 || functions[0].ID != "fn.main" {
		t.Fatalf("retained functions = %#v", functions)
	}
}

func TestPrepareRetainsIntrinsicDynamicResultMethods(t *testing.T) {
	for _, intrinsic := range []ir.IntrinsicID{"reflect.type_of", "reflect.value_of"} {
		t.Run(string(intrinsic), func(t *testing.T) {
			methodSignature := types.FunctionSignature{Results: []types.TypeRef{types.Builtin(types.PrimitiveString)}}
			interfaceKey := types.TypeKey{ModulePath: "reflect", DeclID: "Type"}
			interfaceRef := types.TypeRef{Kind: types.Named, Named: interfaceKey, Node: "named.type"}
			runtimeKey := types.TypeKey{ModulePath: "reflect", DeclID: "runtimeType"}
			runtimeRef := types.TypeRef{Kind: types.Named, Named: runtimeKey, Node: "named.runtimeType"}
			table := types.NewTable(
				types.TypeNode{ID: "named.type", Kind: types.Named, Identity: interfaceKey, Underlying: types.TypeRef{Kind: types.Interface, Node: "interface.type"}},
				types.TypeNode{ID: "interface.type", Kind: types.Interface, Methods: []types.Method{{Name: "Name", Signature: methodSignature}}},
				types.TypeNode{ID: "named.runtimeType", Kind: types.Named, Identity: runtimeKey, Underlying: types.TypeRef{Kind: types.Struct, Node: "struct.runtimeType"}, Methods: []types.Method{{Name: "Name", Signature: methodSignature, FunctionID: "method.runtimeType.Name", Receiver: runtimeRef}}},
				types.TypeNode{ID: "struct.runtimeType", Kind: types.Struct},
			)
			artifact := ir.NewArtifact("reflect", "reflect")
			artifact.TypeTable = *table
			artifact.Functions = []ir.Function{
				{
					ID:     "fn.main",
					Locals: []ir.Local{{ID: "local.type", Type: interfaceRef}},
					Code: linkerTestCode([]types.TypeRef{interfaceRef, types.Builtin(types.PrimitiveString)}, []ir.Instruction{
						{Op: ir.OpCallIntrinsic, Payload: ir.CallIntrinsicPayload{ID: intrinsic, ArgCount: 1, ResultCount: 1}},
						{Op: ir.OpCallInterface, Payload: ir.CallInterfacePayload{InterfaceType: interfaceRef, Method: "Name", ResultCount: 1}},
					}, []ir.SlotOperands{
						{Inputs: []ir.Operand{{Kind: ir.OperandLocal}}, Outputs: []uint32{0}},
						{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Outputs: []uint32{1}, Release: []uint32{0}},
					}),
				},
				{Code: &ir.SlotCode{}, ID: "method.runtimeType.Name", Signature: methodSignature},
			}
			linked, err := retainReachableCode(context.Background(), map[string]ir.Artifact{"reflect": artifact}, []ir.Entry{{Name: "default", ModulePath: "reflect", FunctionID: "fn.main"}})
			if err != nil {
				t.Fatal(err)
			}
			functions := linked["reflect"].Functions
			if len(functions) != 2 || functions[0].ID != "fn.main" || functions[1].ID != "method.runtimeType.Name" {
				t.Fatalf("retained functions = %#v", functions)
			}
		})
	}
}

func TestPrepareRetainsBuiltinErrorMethodForReachableType(t *testing.T) {
	methodSignature := types.FunctionSignature{Results: []types.TypeRef{types.Builtin(types.PrimitiveString)}}
	errorKey := types.TypeKey{ModulePath: "example/main", DeclID: "failure"}
	errorRef := types.TypeRef{Kind: types.Named, Named: errorKey, Node: "named.failure"}
	builtinError := types.TypeRef{Kind: types.Interface, Node: "interface.error"}
	table := types.NewTable(
		types.TypeNode{ID: "interface.error", Kind: types.Interface, Methods: []types.Method{{Name: "Error", Signature: methodSignature}}},
		types.TypeNode{
			ID: "named.failure", Kind: types.Named, Identity: errorKey, Underlying: types.Builtin(types.PrimitiveString),
			Methods: []types.Method{{Name: "Error", Signature: methodSignature, FunctionID: "method.failure.Error", Receiver: errorRef}},
		},
	)
	artifact := ir.NewArtifact("example/main", "main")
	artifact.TypeTable = *table
	artifact.Functions = []ir.Function{
		{Code: &ir.SlotCode{}, ID: "fn.main", Locals: []ir.Local{
			{ID: "local.error", Type: builtinError},
			{ID: "local.failure", Type: errorRef},
		}},
		{Code: &ir.SlotCode{}, ID: "method.failure.Error", Signature: methodSignature},
	}
	linked, err := retainReachableCode(context.Background(), map[string]ir.Artifact{"example/main": artifact}, []ir.Entry{{Name: "default", ModulePath: "example/main", FunctionID: "fn.main"}})
	if err != nil {
		t.Fatal(err)
	}
	functions := linked["example/main"].Functions
	if len(functions) != 2 || functions[0].ID != "fn.main" || functions[1].ID != "method.failure.Error" {
		t.Fatalf("retained functions = %#v", functions)
	}
}

func BenchmarkRetainReachableConstants(b *testing.B) {
	const count = 4096
	artifact := ir.NewArtifact("example/main", "main")
	artifact.TypeTable = *types.NewTable()
	artifact.Constants = make([]ir.Constant, count)
	instructions := make([]ir.Instruction, 0, count*2)
	operands := make([]ir.SlotOperands, 0, count*2)
	for index := range count {
		id := "const." + strconv.Itoa(index)
		artifact.Constants[index] = ir.Constant{
			ID: id, Type: types.Builtin(types.PrimitiveInt), Value: json.RawMessage(strconv.Itoa(index)),
		}
		operands = append(operands, ir.SlotOperands{Outputs: []uint32{0}}, ir.SlotOperands{Inputs: []ir.Operand{{Kind: ir.OperandSlot}}, Release: []uint32{0}})
		instructions = append(instructions,
			ir.Instruction{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: id}},
			ir.Instruction{Op: ir.OpPop},
		)
	}
	artifact.Functions = []ir.Function{{ID: "fn.main", Code: linkerTestCode([]types.TypeRef{types.Builtin(types.PrimitiveInt)}, instructions, operands)}}
	source := map[string]ir.Artifact{"example/main": artifact}
	entries := []ir.Entry{{Name: "default", ModulePath: "example/main", FunctionID: "fn.main"}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := retainReachableCode(context.Background(), source, entries); err != nil {
			b.Fatal(err)
		}
	}
}
