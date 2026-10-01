package emit

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLowerProducesDeclaredConstants(t *testing.T) {
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Constants: []hir.Constant{{
			ID:    "const.answer",
			Name:  "answer",
			Type:  testHIRType("Int64"),
			Value: constant.Scalar(`42`),
		}},
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Int64"),
			Body: []hir.Statement{{
				Kind:    hir.StmtReturn,
				Results: []hir.Expression{{Kind: hir.ExprConst, ConstantID: "const.answer"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if len(artifact.Constants) != 1 || artifact.Constants[0].ID != "const.answer" {
		t.Fatalf("expected declared constant, got %#v", artifact.Constants)
	}
	code := artifact.Functions[0].Code
	inputs := code.Operands[code.Instructions[0].Operands].Inputs
	if code.Instructions[0].Op != ir.OpReturn || len(inputs) != 1 || inputs[0].Kind != ir.OperandConstant || artifact.Constants[inputs[0].Index].ID != "const.answer" {
		t.Fatalf("return does not reference declared constant: %#v", inputs)
	}
}

func TestLowerProducesTypeInstructions(t *testing.T) {
	value := hir.Expression{Kind: hir.ExprLiteral, Type: testHIRType("Int64"), Value: constant.Scalar(`42`)}
	asAny := hir.Expression{Kind: hir.ExprConvert, Type: testHIRType("Any"), Operand: &value}
	asInt := hir.Expression{Kind: hir.ExprTypeAssert, Type: testHIRType("Int64"), Operand: &asAny}
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Int64"),
			Body: []hir.Statement{{
				Kind:    hir.StmtReturn,
				Results: []hir.Expression{asInt},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[0])
	if got := instructions[0].Op; got != ir.OpConvert {
		t.Fatalf("expected convert instruction, got %q", got)
	}
	if got := instructions[1].Op; got != ir.OpTypeAssert {
		t.Fatalf("expected type_assert instruction, got %q", got)
	}
}

func TestLowerProducesGlobalInstructions(t *testing.T) {
	value := hir.Expression{Kind: hir.ExprLiteral, Type: testHIRType("Int64"), Value: constant.Scalar(`42`)}
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Globals:    []hir.Global{{ID: "global.answer", Name: "answer", Type: testHIRType("Int64")}},
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Int64"),
			Body: []hir.Statement{{
				Kind:   hir.StmtStoreGlobal,
				Global: "global.answer",
				Expr:   value,
			}, {
				Kind: hir.StmtReturn,
				Results: []hir.Expression{{
					Kind:   hir.ExprGlobal,
					Global: "global.answer",
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if len(artifact.Globals) != 1 || artifact.Globals[0].ID != "global.answer" {
		t.Fatalf("expected lowered global table, got %#v", artifact.Globals)
	}
	instructions := functionOperations(t, artifact.Functions[0])
	if got := instructions[0].Op; got != ir.OpStoreGlobal {
		t.Fatalf("expected store_global instruction, got %q", got)
	}
	if got := instructions[1].Op; got != ir.OpLoadGlobal {
		t.Fatalf("expected load_global instruction, got %q", got)
	}
}

func TestLowerProducesUpvalueClosureInstructions(t *testing.T) {
	value := hir.Expression{Kind: hir.ExprLiteral, Type: testHIRType("Int64"), Value: constant.Scalar(`41`)}
	one := hir.Expression{Kind: hir.ExprLiteral, Type: testHIRType("Int64"), Value: constant.Scalar(`1`)}
	current := hir.Expression{Kind: hir.ExprUpvalue, Upvalue: "up.x"}
	next := hir.Expression{
		Kind:     hir.ExprBinary,
		Operator: "+",
		Left:     &current,
		Right:    &one,
	}
	closure := hir.Expression{
		Kind:     hir.ExprFunction,
		Function: "fn.inc",
		Captures: []hir.CaptureTarget{{Kind: "local", Local: "local.x"}},
	}
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.inc",
			Name:      "inc",
			Signature: testHIRSignature("function() Int64"),
			Upvalues:  []hir.Upvalue{{ID: "up.x", Name: "x", Type: testHIRType("Int64")}},
			Body: []hir.Statement{{
				Kind:    hir.StmtStoreUpvalue,
				Upvalue: "up.x",
				Expr:    next,
			}, {
				Kind:    hir.StmtReturn,
				Results: []hir.Expression{{Kind: hir.ExprUpvalue, Upvalue: "up.x"}},
			}},
		}, {
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() function() Int64"),
			Locals:    []hir.Local{{ID: "local.x", Name: "x", Type: testHIRType("Int64")}},
			Body: []hir.Statement{{
				Kind:  hir.StmtStoreLocal,
				Local: "local.x",
				Expr:  value,
			}, {
				Kind:    hir.StmtReturn,
				Results: []hir.Expression{closure},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	if got := functionOperations(t, artifact.Functions[0])[0].Op; got != ir.OpLoadUpvalue {
		t.Fatalf("expected load_upvalue instruction, got %q", got)
	}
	if got := functionOperations(t, artifact.Functions[0])[2].Op; got != ir.OpStoreUpvalue {
		t.Fatalf("expected store_upvalue instruction, got %q", got)
	}
	var payload ir.ClosurePayload
	if err := ir.ReadInstructionPayload(functionOperations(t, artifact.Functions[1])[1].Payload, &payload); err != nil {
		t.Fatalf("decode closure payload failed: %v", err)
	}
	if payload.Function != "fn.inc" || len(payload.Captures) != 1 || payload.Captures[0].Local != "local.x" {
		t.Fatalf("unexpected closure payload: %#v", payload)
	}
}
