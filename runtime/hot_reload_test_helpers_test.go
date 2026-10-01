package runtime

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func patchErrorCode(err error) string {
	var patchError PatchError
	if errors.As(err, &patchError) {
		return patchError.Code
	}
	return ""
}

func patchTestProgram(t *testing.T, artifact ir.Artifact, hash string) *Program {
	t.Helper()
	attachRuntimeTestTypeNodes(&artifact)
	code, err := newLoader().load(artifact)
	if err != nil {
		t.Fatal(err)
	}
	modules := map[string]*executable{artifact.Module.Path: code}
	moduleOrder, err := prepareModuleReferences(modules)
	if err != nil {
		t.Fatal(err)
	}
	const entryName = "run"
	return &Program{code: &programCode{
		image: ir.ExecutionImage{
			Root: artifact.Module.Path,
			Hash: hash,
			Entries: []ir.Entry{{
				Name: entryName, ModulePath: artifact.Module.Path, FunctionID: "fn.entry",
			}},
		},
		root: code, modules: modules, moduleOrder: moduleOrder,
		entries: map[string]string{entryName: "fn.entry"},
	}, symbols: testSymbolIndex(&artifact)}
}

func patchMultiModuleProgram(t *testing.T, rootArtifact, dependencyArtifact ir.Artifact, hash string) *Program {
	t.Helper()
	attachRuntimeTestTypeNodes(&rootArtifact)
	attachRuntimeTestTypeNodes(&dependencyArtifact)
	root, err := newLoader().load(rootArtifact)
	if err != nil {
		t.Fatal(err)
	}
	dependency, err := newLoader().load(dependencyArtifact)
	if err != nil {
		t.Fatal(err)
	}
	modules := map[string]*executable{
		rootArtifact.Module.Path:       root,
		dependencyArtifact.Module.Path: dependency,
	}
	moduleOrder, err := prepareModuleReferences(modules)
	if err != nil {
		t.Fatal(err)
	}
	return &Program{code: &programCode{
		image: ir.ExecutionImage{
			Root: rootArtifact.Module.Path, Hash: hash,
			Entries: []ir.Entry{{Name: "run", ModulePath: rootArtifact.Module.Path, FunctionID: "fn.entry"}},
		},
		root: root, modules: modules, moduleOrder: moduleOrder,
		entries: map[string]string{"run": "fn.entry"},
	}, symbols: testSymbolIndex(&rootArtifact)}
}

func patchCallArtifact(base, value int64) ir.Artifact {
	artifact := ir.NewArtifact("patch/main", "main")
	artifact.Constants = []ir.Constant{
		{ID: "const.base", Type: testType("Int64"), Value: json.RawMessage(jsonInt(base))},
		{ID: "const.value", Type: testType("Int64"), Value: json.RawMessage(jsonInt(value))},
	}
	entry := ir.Function{ID: "fn.entry", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
		{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.base"}},
	}, [][2][]uint32{{nil, {0}}})}
	entry.Code.Types = append(entry.Code.Types, testType("Int64"), testType("Int64"))
	insertTestDelay(entry.Code, len(entry.Code.Instructions), 2)
	appendTestSlotCode(entry.Code, []ir.Instruction{
		{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.value", ResultCount: 1}},
		{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "+"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
	}, [][2][]uint32{{nil, {1}}, {{0, 1}, {2}}, {{2}, nil}})
	artifact.Functions = []ir.Function{
		entry,
		{ID: "fn.value", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.value"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}})},
	}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func patchTailCallArtifact(value int64) ir.Artifact {
	artifact := ir.NewArtifact("patch/tail", "main")
	artifact.Constants = []ir.Constant{{ID: "const.value", Type: testType("Int64"), Value: json.RawMessage(jsonInt(value))}}
	entry := ir.Function{Code: &ir.SlotCode{}, ID: "fn.entry", Signature: testSignature("function() Int64")}
	insertTestDelay(entry.Code, 0, 2)
	appendTestSlotCode(entry.Code, []ir.Instruction{{Op: ir.OpTailCallDirect, Payload: ir.CallPayload{Function: "fn.value", ResultCount: 1}}}, [][2][]uint32{{nil, nil}})
	artifact.Functions = []ir.Function{
		entry,
		{ID: "fn.value", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.value"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}})},
	}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func patchMultiRootArtifact(base int64) ir.Artifact {
	artifact := ir.NewArtifact("patch/root", "main")
	artifact.Constants = []ir.Constant{{ID: "const.base", Type: testType("Int64"), Value: json.RawMessage(jsonInt(base))}}
	artifact.Requirements = []ir.Requirement{{Kind: "source", ModulePath: "patch/dep"}}
	entry := ir.Function{ID: "fn.entry", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
		{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.base"}},
	}, [][2][]uint32{{nil, {0}}})}
	entry.Code.Types = append(entry.Code.Types, testType("Int64"), testType("Int64"))
	insertTestDelay(entry.Code, len(entry.Code.Instructions), 2)
	appendTestSlotCode(entry.Code, []ir.Instruction{
		{Op: ir.OpCallDirect, Payload: ir.CallPayload{ModulePath: "patch/dep", Function: "fn.value", ResultCount: 1}},
		{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "+"}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
	}, [][2][]uint32{{nil, {1}}, {{0, 1}, {2}}, {{2}, nil}})
	artifact.Functions = []ir.Function{entry}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func patchMultiDependencyArtifact(value int64) ir.Artifact {
	artifact := ir.NewArtifact("patch/dep", "dep")
	artifact.Constants = []ir.Constant{{ID: "const.value", Type: testType("Int64"), Value: json.RawMessage(jsonInt(value))}}
	artifact.Functions = []ir.Function{{
		ID: "fn.value", Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.value"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Value", Kind: "function", ID: "fn.value"}}
	return artifact
}

func patchClosureArtifact(value int64) ir.Artifact {
	artifact := ir.NewArtifact("patch/closure", "main")
	artifact.Constants = []ir.Constant{{ID: "const.value", Type: testType("Int64"), Value: json.RawMessage(jsonInt(value))}}
	entry := ir.Function{ID: "fn.entry", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"function() Int64"}, []ir.Instruction{{
		Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "fn.literal.1"},
	}}, [][2][]uint32{{nil, {0}}})}
	entry.Code.Types = append(entry.Code.Types, testType("Int64"))
	insertTestDelay(entry.Code, len(entry.Code.Instructions), 2)
	appendTestSlotCode(entry.Code, []ir.Instruction{
		{Op: ir.OpCallValue, Payload: ir.CallPayload{ResultCount: 1}},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
	}, [][2][]uint32{{{0}, {1}}, {{1}, nil}})
	artifact.Functions = []ir.Function{
		entry,
		{ID: "fn.literal.1", RevisionLocal: true, Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.value"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}})},
	}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func patchGlobalArtifact(delta int64) ir.Artifact {
	artifact := ir.NewArtifact("patch/global", "main")
	artifact.Constants = []ir.Constant{{ID: "const.delta", Type: testType("Int64"), Value: json.RawMessage(jsonInt(delta))}}
	artifact.Globals = []ir.Global{{ID: "global.total", Type: testType("Int64")}}
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() Int64"),
		Code: testSlotCode([]string{"Int64", "Int64", "Int64"}, []ir.Instruction{
			{Op: ir.OpLoadGlobal, Payload: ir.GlobalPayload{Global: "global.total"}},
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.delta"}},
			{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "+"}},
			{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.total"}},
			{Op: ir.OpLoadGlobal, Payload: ir.GlobalPayload{Global: "global.total"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2}}, {{2}, nil}, {nil, {0}}, {{0}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func patchPanicArtifact(message string) ir.Artifact {
	artifact := ir.NewArtifact("patch/panic", "main")
	data, _ := json.Marshal(message)
	artifact.Constants = []ir.Constant{{ID: "const.message", Type: testType("String"), Value: data}}
	entry := ir.Function{Code: &ir.SlotCode{}, ID: "fn.entry", Signature: testSignature("function() Void")}
	entry.Code.Types = append(entry.Code.Types, testType("String"))
	insertTestDelay(entry.Code, 0, 2)
	appendTestSlotCode(entry.Code, []ir.Instruction{
		{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.message"}},
		{Op: ir.OpPanic},
	}, [][2][]uint32{{nil, {0}}, {{0}, nil}})
	artifact.Functions = []ir.Function{entry}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func patchFFIArtifact() ir.Artifact {
	artifact := ir.NewArtifact("patch/ffi", "main")
	artifact.Constants = []ir.Constant{
		{ID: "const.route", Type: testType("String"), Value: json.RawMessage(`"patch"`)},
		{ID: "const.payload", Type: testType("Slice<Uint8>"), Value: json.RawMessage(`null`)},
	}
	artifact.Functions = []ir.Function{{
		ID: "fn.entry", Signature: testSignature("function() tuple(Slice<Uint8>, String, Int)"),
		Code: testSlotCode([]string{"String", "Slice<Uint8>", "Slice<Uint8>", "String", "Int"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.route"}},
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.payload"}},
			{Op: ir.OpCallFFI, Payload: ir.CallFFIPayload{ArgCount: 2, ResultCount: 3}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 3}},
		}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, {2, 3, 4}}, {{2, 3, 4}, nil}}),
	}}
	artifact.Exports = []ir.Export{{Name: "Run", Kind: "function", ID: "fn.entry"}}
	return artifact
}

func jsonInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
