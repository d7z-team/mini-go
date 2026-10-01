package emit

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLowerProducesFunctionSpawnInstructions(t *testing.T) {
	child := hir.Expression{Kind: hir.ExprFunction, Function: "fn.child"}
	_, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.child",
			Name:      "child",
			Signature: testHIRSignature("function() Void"),
		}, {
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
			Body: []hir.Statement{{
				Kind: hir.StmtSpawn,
				Expr: child,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
}

func TestLowerProducesImportAndPanicInstructions(t *testing.T) {
	message := hir.Expression{Kind: hir.ExprLiteral, Type: testHIRType("String"), Value: constant.String("failed", "String", false).Ref()}
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Requirements: []hir.Requirement{{
			Kind:       "source",
			ModulePath: "example/lib",
		}},
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
			Body: []hir.Statement{{
				Kind:   hir.StmtInitModule,
				Module: "example/lib",
			}, {
				Kind: hir.StmtPanic,
				Expr: message,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[0])
	if got := instructions[0].Op; got != ir.OpInitModule {
		t.Fatalf("expected init_module instruction, got %q", got)
	}
	if got := instructions[1].Op; got != ir.OpPanic {
		t.Fatalf("expected panic instruction, got %q", got)
	}
}

func TestLowerProducesDeferInstruction(t *testing.T) {
	deferred := hir.Expression{Kind: hir.ExprFunction, Function: "fn.cleanup"}
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.cleanup",
			Name:      "cleanup",
			Signature: testHIRSignature("function() Void"),
		}, {
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
			Body: []hir.Statement{{
				Kind: hir.StmtDefer,
				Expr: deferred,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[1])
	if got := instructions[1].Op; got != ir.OpDeferPush {
		t.Fatalf("expected defer_push instruction, got %q", got)
	}
}

func TestLowerProducesRecoverExpression(t *testing.T) {
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Any"),
			Body: []hir.Statement{{
				Kind:    hir.StmtReturn,
				Results: []hir.Expression{{Kind: hir.ExprRecover}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[0])
	if got := instructions[0].Op; got != ir.OpRecover {
		t.Fatalf("expected recover instruction, got %q", got)
	}
}
