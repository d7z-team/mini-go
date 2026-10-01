package runtime

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPreparedInstructionBindsOperator(t *testing.T) {
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpBinary, Payload: ir.OperatorPayload{Operator: "+"}})
	if err != nil {
		t.Fatal(err)
	}
	if instruction.op != preparedBinary || instruction.operator != operatorAdd {
		t.Fatalf("prepared instruction = %#v", instruction)
	}
	if instruction.control {
		t.Fatal("ordinary binary instruction was classified as scheduler control")
	}
}

func TestPreparedInstructionBindsRuntimeTypeAndStructSchema(t *testing.T) {
	table := &types.TypeTable{}
	ref, err := types.NewParser("example/module", table).Parse("struct{Value:Int}")
	if err != nil {
		t.Fatal(err)
	}
	payload := ir.MakeStructPayload{Type: ref, Fields: []string{"Value"}}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpMakeStruct, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	instruction.bindTypes(table, "example/module")
	if got := instruction.runtimeType.String(); got != "struct{Value:Int}" {
		t.Fatalf("prepared type = %q", got)
	}
	if instruction.structSchema == nil || len(instruction.structSchema.fields) != 1 || instruction.structSchema.fields[0].Name != "Value" {
		t.Fatalf("prepared schema = %#v", instruction.structSchema)
	}
}

func TestPreparedDirectCallUsesCurrentModuleByDefault(t *testing.T) {
	payload := ir.CallPayload{Function: "fn.target"}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpCallDirect, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	instruction.bindTypes(&types.TypeTable{}, "example/module")
	if instruction.callModule != "example/module" {
		t.Fatalf("prepared call module = %q", instruction.callModule)
	}
	if !instruction.control {
		t.Fatal("direct call was not classified as scheduler control")
	}
}

func TestPreparedTailCallIsSchedulerControl(t *testing.T) {
	payload := ir.CallPayload{Function: "fn.target", ResultCount: 1}
	instruction, err := prepareInstruction(ir.Instruction{Op: ir.OpTailCallDirect, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if instruction.op != preparedTailCallDirect || !instruction.control {
		t.Fatalf("prepared tail call = %#v", instruction)
	}
}
