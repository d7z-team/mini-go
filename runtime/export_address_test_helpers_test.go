package runtime

import ir "github.com/d7z-team/mini-go/runtime/bytecode"

func exportAddressArtifacts() (ir.Artifact, ir.Artifact) {
	root := ir.NewArtifact("address/root", "main")
	root.Requirements = []ir.Requirement{{Kind: "source", ModulePath: "address/state", Exports: []string{"Item"}}}
	root.Globals = []ir.Global{{ID: "global.saved", Type: testType("Ptr<Int64>")}}
	root.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"Ptr<Int64>", "Int64"}, []ir.Instruction{
		{Op: ir.OpAddressOf, Payload: ir.AddressPayload{Kind: "export", ModulePath: "address/state", Export: "Item", Path: []ir.AddressPathSegment{{Kind: "field", Field: "N"}}}},
		{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.saved"}},
		{Op: ir.OpLoadGlobal, Payload: ir.GlobalPayload{Global: "global.saved"}},
		{Op: ir.OpLoadIndirect},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
	}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {0}}, {{0}, {1}}, {{1}, nil}})}}
	dependency := ir.NewArtifact("address/state", "state")
	dependency.Globals = []ir.Global{{ID: "global.item", Type: testType("struct{N:Int64}")}}
	dependency.Exports = []ir.Export{{Name: "Item", Kind: "global", ID: "global.item", Type: testType("struct{N:Int64}")}}
	dependency.Constants = []ir.Constant{{ID: "const.initial", Type: testType("Int64"), Value: []byte("1")}}
	dependency.Functions = []ir.Function{{ID: "fn.init", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"Ptr<Int64>", "Int64"}, []ir.Instruction{
		{Op: ir.OpAddressOf, Payload: ir.AddressPayload{Kind: "global", Global: "global.item", Path: []ir.AddressPathSegment{{Kind: "field", Field: "N"}}}},
		{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.initial"}},
		{Op: ir.OpStoreIndirect},
		{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
	}, [][2][]uint32{{nil, {0}}, {nil, {1}}, {{0, 1}, nil}, {nil, nil}})}}
	return root, dependency
}
