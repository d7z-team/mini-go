package emit

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

var testHIRTypes types.TypeTable

func functionOperations(t *testing.T, function ir.Function) []ir.Instruction {
	t.Helper()
	operations, err := function.Operations()
	if err != nil {
		t.Fatal(err)
	}
	return operations
}

func testHIRSignature(text string) types.FunctionSignature {
	parser := types.NewParser("test", &testHIRTypes)
	ref, err := parser.Parse(text)
	if err != nil {
		panic(err)
	}
	signature, ok := parser.Table.IsFunction(ref)
	if !ok {
		panic("test HIR signature is not a function")
	}
	return signature
}

func testHIRType(text string) types.TypeRef {
	parser := types.NewParser("test", &testHIRTypes)
	ref, err := parser.Parse(text)
	if err != nil {
		panic(err)
	}
	return ref
}

func lowerTestProgram(t *testing.T, program hir.Program) (ir.Artifact, error) {
	t.Helper()
	for _, node := range testHIRTypes.Nodes {
		if _, exists := program.TypeTable.Node(types.TypeRef{Kind: node.Kind, Node: node.ID}); exists {
			continue
		}
		if err := program.TypeTable.Add(node); err != nil {
			t.Fatalf("attach test HIR type: %v", err)
		}
	}
	return Lower(program)
}
