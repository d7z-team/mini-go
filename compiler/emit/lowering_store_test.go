package emit

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestLowerUsesExpressionResultCounts(t *testing.T) {
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.log",
			Name:      "log",
			Signature: testHIRSignature("function() Void"),
		}, {
			ID:        "fn.pair",
			Name:      "pair",
			Signature: testHIRSignature("function() tuple(Int64, Int64)"),
		}, {
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() tuple(Int64, Int64)"),
			Body: []hir.Statement{{
				Kind: hir.StmtExpr,
				Expr: hir.Expression{
					Kind:        hir.ExprCallDirect,
					Function:    "fn.log",
					ResultCount: 0,
				},
			}, {
				Kind: hir.StmtReturn,
				Results: []hir.Expression{{
					Kind:        hir.ExprCallDirect,
					Function:    "fn.pair",
					ResultCount: 2,
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[2])
	if len(instructions) != 3 {
		t.Fatalf("expected call, call, return with no pop, got %#v", instructions)
	}
	var call ir.CallPayload
	if err := ir.ReadInstructionPayload(instructions[0].Payload, &call); err != nil {
		t.Fatalf("unmarshal void call payload failed: %v", err)
	}
	if call.ResultCount != 0 {
		t.Fatalf("expected void call result count 0, got %d", call.ResultCount)
	}
	var ret ir.ReturnPayload
	if err := ir.ReadInstructionPayload(instructions[2].Payload, &ret); err != nil {
		t.Fatalf("unmarshal return payload failed: %v", err)
	}
	if ret.ResultCount != 2 {
		t.Fatalf("expected return result count 2, got %d", ret.ResultCount)
	}
}

func TestLowerStoreResultsPreservesResultAndDestinationOrder(t *testing.T) {
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.pair",
			Name:      "pair",
			Signature: testHIRSignature("function() tuple(Int64, Int64)"),
		}, {
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
			Locals: []hir.Local{
				{ID: "local.a", Name: "a", Type: testHIRType("Int64")},
				{ID: "local.b", Name: "b", Type: testHIRType("Int64")},
			},
			Body: []hir.Statement{{
				Kind: hir.StmtStoreResults,
				Expr: hir.Expression{
					Kind:        hir.ExprCallDirect,
					Function:    "fn.pair",
					ResultCount: 2,
				},
				Targets: []hir.StoreTarget{
					{Kind: "local", Local: "local.a"},
					{Kind: "local", Local: "local.b"},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[1])
	if len(instructions) != 3 {
		t.Fatalf("expected call and two stores, got %#v", instructions)
	}
	if instructions[0].Op != ir.OpCallDirect || instructions[1].Op != ir.OpStoreLocal || instructions[2].Op != ir.OpStoreLocal {
		t.Fatalf("expected call_direct/store_local/store_local, got %#v", instructions)
	}
	var firstStore ir.LocalPayload
	if err := ir.ReadInstructionPayload(instructions[1].Payload, &firstStore); err != nil {
		t.Fatalf("unmarshal first store failed: %v", err)
	}
	var secondStore ir.LocalPayload
	if err := ir.ReadInstructionPayload(instructions[2].Payload, &secondStore); err != nil {
		t.Fatalf("unmarshal second store failed: %v", err)
	}
	if firstStore.Local != "local.a" || secondStore.Local != "local.b" {
		t.Fatalf("expected source store order a,b, got %q,%q", firstStore.Local, secondStore.Local)
	}
	code := artifact.Functions[1].Code
	results := code.Operands[code.Instructions[0].Operands].Outputs
	for i := range results {
		input := code.Operands[code.Instructions[i+1].Operands].Inputs[0]
		if input.Kind != ir.OperandSlot || input.Index != results[i] {
			t.Fatalf("result %d routed to the wrong destination: %v", i, input)
		}
	}
}

func TestLowerStoreValuesPreservesRightHandSnapshots(t *testing.T) {
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
			Locals: []hir.Local{
				{ID: "local.a", Name: "a", Type: testHIRType("Int64")},
				{ID: "local.b", Name: "b", Type: testHIRType("Int64")},
			},
			Body: []hir.Statement{{
				Kind: hir.StmtStoreValues,
				Values: []hir.Expression{
					{Kind: hir.ExprLocal, Local: "local.b"},
					{Kind: hir.ExprLocal, Local: "local.a"},
				},
				Targets: []hir.StoreTarget{
					{Kind: "local", Local: "local.a"},
					{Kind: "local", Local: "local.b"},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	instructions := functionOperations(t, artifact.Functions[0])
	code := artifact.Functions[0].Code
	snapshotPC, firstStorePC, lastStorePC := -1, -1, -1
	var snapshot uint32
	for pc, instruction := range instructions {
		payload, ok := instruction.Payload.(ir.LocalPayload)
		if !ok {
			continue
		}
		if instruction.Op == ir.OpLoadLocal && payload.Local == "local.a" {
			snapshotPC = pc
			snapshot = code.Operands[code.Instructions[pc].Operands].Outputs[0]
		}
		if instruction.Op == ir.OpStoreLocal {
			if payload.Local == "local.a" {
				firstStorePC = pc
			} else {
				lastStorePC = pc
			}
		}
	}
	if snapshotPC < 0 || snapshotPC >= firstStorePC || firstStorePC >= lastStorePC {
		t.Fatalf("right-hand snapshot must precede source-ordered writes: %v", instructions)
	}
	input := code.Operands[code.Instructions[lastStorePC].Operands].Inputs[0]
	if input.Kind != ir.OperandSlot || input.Index != snapshot {
		t.Fatalf("second write rereads the overwritten local: %v", input)
	}
}

func TestLowerStoreLocalPreservesRebindPayload(t *testing.T) {
	literal := hir.Expression{Kind: hir.ExprLiteral, Type: testHIRType("Int64"), Value: constant.Scalar(`1`)}
	artifact, err := lowerTestProgram(t, hir.Program{
		ModulePath: "example/module",
		Package:    "main",
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
			Locals:    []hir.Local{{ID: "local.x", Name: "x", Type: testHIRType("Int64")}},
			Body: []hir.Statement{
				{Kind: hir.StmtStoreLocal, Local: "local.x", Expr: literal},
				{Kind: hir.StmtStoreLocal, Local: "local.x", Rebind: true, Expr: literal},
				{Kind: hir.StmtReturn},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Lower failed: %v", err)
	}
	fn := artifact.Functions[0]
	var stores []ir.LocalPayload
	for _, inst := range functionOperations(t, fn) {
		if inst.Op != ir.OpStoreLocal {
			continue
		}
		var payload ir.LocalPayload
		if err := ir.ReadInstructionPayload(inst.Payload, &payload); err != nil {
			t.Fatalf("unmarshal store payload failed: %v", err)
		}
		stores = append(stores, payload)
	}
	if len(stores) != 2 {
		t.Fatalf("expected two store_local instructions, got %#v", functionOperations(t, fn))
	}
	if stores[0].Rebind {
		t.Fatalf("ordinary store_local should not rebind: %#v", stores[0])
	}
	if !stores[1].Rebind {
		t.Fatalf("declaration store_local should preserve rebind: %#v", stores[1])
	}
}
