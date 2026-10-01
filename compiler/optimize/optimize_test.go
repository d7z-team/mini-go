package optimize

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestApplyFoldsConstantBranchAndPreservesSelectedSourcePoints(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{
		ID: "fn.main",
		Body: []hir.Statement{
			{Kind: hir.StmtJumpIf, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`true`)}, Label: "then", SourcePoints: []hir.Location{{File: "main.mgo", Line: 1, Column: 1}}},
			{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`0`)}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 2, Column: 2}}},
			{Kind: hir.StmtJump, Label: "end"},
			{Kind: hir.StmtLabel, Label: "then"},
			{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`1`)}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 3, Column: 2}}},
			{Kind: hir.StmtLabel, Label: "end"},
			{Kind: hir.StmtReturn},
		},
	}}}

	optimized, err := Apply(program, LevelDefault)
	if err != nil {
		t.Fatal(err)
	}
	body := optimized.Functions[0].Body
	for _, statement := range body {
		if statement.Kind == hir.StmtJump || statement.Kind == hir.StmtJumpIf || statement.Kind == hir.StmtLabel {
			t.Fatalf("redundant control statement remained: %#v", statement)
		}
		for _, location := range statement.SourcePoints {
			if location.Line == 2 {
				t.Fatalf("unreachable branch source point remained: %#v", body)
			}
		}
	}
	if len(body) != 2 || body[0].Kind != hir.StmtExpr || body[1].Kind != hir.StmtReturn {
		t.Fatalf("optimized body = %#v", body)
	}
	if got := body[0].SourcePoints; len(got) != 2 || got[0].Line != 1 || got[1].Line != 3 {
		t.Fatalf("selected source points = %#v", got)
	}
}

func TestBranchFallthroughPreservesComparisonAndOtherLabelEntries(t *testing.T) {
	for _, negated := range []bool{false, true} {
		comparison := hir.Expression{Kind: hir.ExprBinary, Operator: "<", Left: &hir.Expression{Kind: hir.ExprLocal, Local: "a"}, Right: &hir.Expression{Kind: hir.ExprLocal, Local: "b"}}
		body := []hir.Statement{
			{Kind: hir.StmtJumpIf, Expr: comparison, Label: "body", BranchNegated: negated},
			{Kind: hir.StmtJump, Label: "exit"},
			{Kind: hir.StmtLabel, Label: "alias"},
			{Kind: hir.StmtLabel, Label: "body"},
			{Kind: hir.StmtReturn},
			{Kind: hir.StmtLabel, Label: "exit"},
			{Kind: hir.StmtJump, Label: "alias"},
		}
		result, changed := simplifyBranchFallthrough(body)
		if !changed || result[0].Label != "exit" || result[0].BranchNegated == negated || !reflect.DeepEqual(result[0].Expr, comparison) {
			t.Fatalf("incorrect branch polarity: %#v", result[0])
		}
		if result[1].Label != "alias" || result[2].Label != "body" || result[len(result)-1].Label != "alias" {
			t.Fatal("branch simplification lost an alternate entry")
		}
	}
}

func TestConstantBranchRespectsNegatedPolarity(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		body, _ := foldConstantBranches([]hir.Statement{
			{Kind: hir.StmtJumpIf, Label: "target", BranchNegated: true, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(value)}},
			{Kind: hir.StmtReturn},
		}, nil)
		if value == "false" && (body[0].Kind != hir.StmtJump || body[0].BranchNegated) || value == "true" && body[0].Kind != hir.StmtReturn {
			t.Fatalf("negated %s: %#v", value, body)
		}
	}
}

func TestApplyMarksOnlyLogicalDirectTailCalls(t *testing.T) {
	signature := types.FunctionSignature{Results: []types.TypeRef{types.Builtin(types.PrimitiveInt)}}
	program := hir.Program{Functions: []hir.Function{
		{ID: "fn.target", Signature: signature, Body: []hir.Statement{{Kind: hir.StmtReturn}}},
		{ID: "fn.wrapper", Signature: signature, Body: []hir.Statement{{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprCallDirect, Function: "fn.target", ResultCount: 1}}}}},
		{ID: "fn.literal", RevisionLocal: true, Signature: signature, Body: []hir.Statement{{Kind: hir.StmtReturn}}},
		{ID: "fn.closure_wrapper", Signature: signature, Body: []hir.Statement{{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprCallDirect, Function: "fn.literal", ResultCount: 1}}}}},
	}}
	optimized, err := Apply(program, LevelDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got := optimized.Functions[1].Body[0].Kind; got != hir.StmtTailCallDirect {
		t.Fatalf("logical wrapper statement = %s", got)
	}
	if got := optimized.Functions[3].Body[0].Kind; got != hir.StmtReturn {
		t.Fatalf("revision-local wrapper statement = %s", got)
	}
}

func TestApplyLevelsHaveDistinctDeterministicPipelines(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{
		{ID: "fn.main", Body: []hir.Statement{
			{Kind: hir.StmtStoreLocal, Local: "local.condition", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`true`)}},
			{Kind: hir.StmtJumpIf, Label: "then", Expr: hir.Expression{Kind: hir.ExprLocal, Local: "local.condition"}},
			{Kind: hir.StmtReturn},
			{Kind: hir.StmtLabel, Label: "then"},
			{Kind: hir.StmtReturn},
		}},
		{ID: "fn.constant", Body: []hir.Statement{
			{Kind: hir.StmtJumpIf, Label: "then", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`true`)}},
			{Kind: hir.StmtReturn},
			{Kind: hir.StmtLabel, Label: "then"},
			{Kind: hir.StmtReturn},
		}},
	}}

	none, err := Apply(program, LevelNone)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(none, program) {
		t.Fatalf("O0 changed lowered HIR: %#v", none.Functions[0].Body)
	}
	defaultLevel, err := Apply(program, LevelDefault)
	if err != nil {
		t.Fatal(err)
	}
	full, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(defaultLevel, none) || reflect.DeepEqual(full, defaultLevel) {
		t.Fatalf("optimization levels are not distinct:\nO0 %#v\nO1 %#v\nO2 %#v", none.Functions[0].Body, defaultLevel.Functions[0].Body, full.Functions[0].Body)
	}
	for _, statement := range full.Functions[0].Body {
		if statement.Kind == hir.StmtJumpIf {
			t.Fatalf("O2 retained propagated constant branch: %#v", full.Functions[0].Body)
		}
	}
}

func TestApplyRejectsUnknownLevel(t *testing.T) {
	if _, err := Apply(hir.Program{}, LevelFull+1); err == nil {
		t.Fatal("unknown optimization level was accepted")
	}
}

func TestFullOptimizationPropagatesSingleResultBooleanWithoutRemovingRebind(t *testing.T) {
	store := hir.Statement{Kind: hir.StmtStoreResults, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`false`)}, Targets: []hir.StoreTarget{{Kind: "local", Local: "flag", Rebind: true}}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 2}}}
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		store,
		{Kind: hir.StmtJumpIf, Expr: hir.Expression{Kind: hir.ExprLocal, Local: "flag"}, Label: "unreachable", SourcePoints: []hir.Location{{File: "main.mgo", Line: 3}}},
		{Kind: hir.StmtReturn, SourcePoints: []hir.Location{{File: "main.mgo", Line: 4}}},
		{Kind: hir.StmtLabel, Label: "unreachable"},
		{Kind: hir.StmtReturn, SourcePoints: []hir.Location{{File: "main.mgo", Line: 5}}},
	}}}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	body := optimized.Functions[0].Body
	if !reflect.DeepEqual(body[0], store) {
		t.Fatalf("rebind changed: %+v", body)
	}
	for _, statement := range body {
		if statement.Kind == hir.StmtJumpIf {
			t.Fatalf("literal condition was not propagated: %+v", body)
		}
		for _, location := range statement.SourcePoints {
			if location.Line == 5 {
				t.Fatalf("unreachable source point retained: %+v", body)
			}
		}
	}
}

func TestFullOptimizationFoldsPureBooleanAndDropsUnusedPureStore(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		{Kind: hir.StmtStoreLocal, Local: "local.unused", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`true`)}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 1}}},
		{Kind: hir.StmtJumpIf, Label: "done", Expr: hir.Expression{
			Kind: hir.ExprBinary, Operator: "&&",
			Left:  &hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`true`)},
			Right: &hir.Expression{Kind: hir.ExprUnary, Operator: "!", Operand: &hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`false`)}},
		}},
		{Kind: hir.StmtReturn},
		{Kind: hir.StmtLabel, Label: "done"},
		{Kind: hir.StmtReturn},
	}}}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	if body := optimized.Functions[0].Body; len(body) != 1 || body[0].Kind != hir.StmtReturn || len(body[0].SourcePoints) != 1 {
		t.Fatalf("O2 body = %#v", body)
	}
}

func TestFullOptimizationKeepsObservableStores(t *testing.T) {
	for _, kind := range []hir.ExpressionKind{hir.ExprCallDirect, hir.ExprCallValue, hir.ExprCallFFI, hir.ExprCallIntrinsic, hir.ExprChanRecv} {
		t.Run(string(kind), func(t *testing.T) {
			program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
				{Kind: hir.StmtStoreLocal, Local: "local.unused", Expr: hir.Expression{Kind: kind}},
				{Kind: hir.StmtReturn},
			}}}}
			optimized, err := Apply(program, LevelFull)
			if err != nil {
				t.Fatal(err)
			}
			if body := optimized.Functions[0].Body; len(body) != 2 || body[0].Kind != hir.StmtStoreLocal {
				t.Fatalf("O2 removed observable %s expression: %#v", kind, body)
			}
		})
	}
}

func TestFullOptimizationEliminatesDeadStoresByControlFlowLiveness(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		{Kind: hir.StmtStoreLocal, Local: "local.value", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`1`)}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 1}}},
		{Kind: hir.StmtStoreLocal, Local: "local.value", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`2`)}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 2}}},
		{Kind: hir.StmtStoreLocal, Local: "local.copy", Expr: hir.Expression{Kind: hir.ExprLocal, Local: "local.value"}, SourcePoints: []hir.Location{{File: "main.mgo", Line: 3}}},
		{Kind: hir.StmtReturn},
	}}}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	body := optimized.Functions[0].Body
	if len(body) != 1 || body[0].Kind != hir.StmtReturn || len(body[0].SourcePoints) != 3 {
		t.Fatalf("O2 dead-store body = %#v", body)
	}
}

func TestFullOptimizationRetainsStoresLiveOnBranchesAndLoops(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		{Kind: hir.StmtStoreLocal, Local: "local.value", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`1`)}},
		{Kind: hir.StmtLabel, Label: "loop"},
		{Kind: hir.StmtJumpIf, Label: "done", Expr: hir.Expression{Kind: hir.ExprLocal, Local: "local.condition"}},
		{Kind: hir.StmtStoreLocal, Local: "local.value", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`2`)}},
		{Kind: hir.StmtJump, Label: "loop"},
		{Kind: hir.StmtLabel, Label: "done"},
		{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprLocal, Local: "local.value"}}},
	}}}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	stores := 0
	for _, statement := range optimized.Functions[0].Body {
		if statement.Kind == hir.StmtStoreLocal && statement.Local == "local.value" {
			stores++
		}
	}
	if stores != 2 {
		t.Fatalf("O2 removed a path-live store: %#v", optimized.Functions[0].Body)
	}
}

func TestFullOptimizationRetainsNamedResultsAcrossPanic(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", ResultLocals: []string{"result"}, Body: []hir.Statement{
		{Kind: hir.StmtStoreLocal, Local: "result", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`7`)}},
		{Kind: hir.StmtPanic, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`1`)}},
	}}}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	body := optimized.Functions[0].Body
	if len(body) != 2 || body[0].Kind != hir.StmtStoreLocal || body[0].Local != "result" {
		t.Fatalf("panic path lost its observable named result: %#v", body)
	}
}

func TestFullOptimizationRetainsEscapedAndReboundLocals(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		{Kind: hir.StmtStoreLocal, Local: "local.addressed", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`1`)}},
		{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprAddressOf, Local: "local.addressed"}},
		{Kind: hir.StmtStoreLocal, Local: "local.captured", Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`2`)}},
		{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprFunction, Captures: []hir.CaptureTarget{{Local: "local.captured"}}}},
		{Kind: hir.StmtStoreLocal, Local: "local.rebound", Rebind: true, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`3`)}},
		{Kind: hir.StmtReturn},
	}}}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	stores := 0
	for _, statement := range optimized.Functions[0].Body {
		if statement.Kind == hir.StmtStoreLocal {
			stores++
		}
	}
	if stores != 3 {
		t.Fatalf("O2 removed escaped or rebound stores: %#v", optimized.Functions[0].Body)
	}
}

func TestApplyIsIdempotentAndDoesNotMutateInput(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		{Kind: hir.StmtJump, Label: "next", SourcePoints: []hir.Location{{File: "main.mgo", Line: 1, Column: 1}}},
		{Kind: hir.StmtLabel, Label: "next"},
		{Kind: hir.StmtReturn},
	}}}}
	original := cloneStatements(program.Functions[0].Body)
	first, err := Apply(program, LevelDefault)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Apply(first, LevelDefault)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("optimizer is not idempotent:\nfirst %#v\nsecond %#v", first, second)
	}
	if !reflect.DeepEqual(program.Functions[0].Body, original) {
		t.Fatalf("optimizer mutated its input: %#v", program.Functions[0].Body)
	}
}

func TestApplyRejectsMalformedLabels(t *testing.T) {
	program := hir.Program{Functions: []hir.Function{{ID: "fn.main", Body: []hir.Statement{
		{Kind: hir.StmtLabel, Label: "same"},
		{Kind: hir.StmtLabel, Label: "same"},
	}}}}
	if _, err := Apply(program, LevelDefault); err == nil {
		t.Fatal("expected duplicate label error")
	}

	program.Functions[0].Body = []hir.Statement{{Kind: hir.StmtJump, Label: "missing"}}
	if _, err := Apply(program, LevelDefault); err == nil {
		t.Fatal("expected unknown label error")
	}
}

func FuzzApplyDeterministic(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256 {
			t.Skip()
		}
		body := make([]hir.Statement, 0, len(data)+1)
		for index, value := range data {
			label := string(rune('a' + value%8))
			local := "local." + label
			switch value % 8 {
			case 0:
				body = append(body, hir.Statement{Kind: hir.StmtLabel, Label: label})
			case 1:
				body = append(body, hir.Statement{Kind: hir.StmtJump, Label: label})
			case 2:
				body = append(body, hir.Statement{Kind: hir.StmtJumpIf, Label: label, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`true`)}})
			case 3:
				body = append(body, hir.Statement{Kind: hir.StmtExpr, SourcePoints: []hir.Location{{File: "f.mgo", Line: index + 1}}})
			case 4:
				body = append(body, hir.Statement{Kind: hir.StmtReturn})
			case 5:
				body = append(body, hir.Statement{Kind: hir.StmtPanic})
			case 6:
				body = append(body, hir.Statement{Kind: hir.StmtStoreLocal, Local: local, Expr: hir.Expression{Kind: hir.ExprLiteral, Value: constant.Scalar(`1`)}})
			case 7:
				body = append(body, hir.Statement{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprLocal, Local: local}})
			}
		}
		body = append(body, hir.Statement{Kind: hir.StmtReturn})
		program := hir.Program{Functions: []hir.Function{{ID: "fn.fuzz", Body: body}}}
		original := cloneStatements(body)
		for level := LevelNone; level <= LevelFull; level++ {
			first, firstErr := Apply(program, level)
			second, secondErr := Apply(program, level)
			if (firstErr == nil) != (secondErr == nil) || firstErr != nil && firstErr.Error() != secondErr.Error() {
				t.Fatalf("O%d non-deterministic errors: %v != %v", level, firstErr, secondErr)
			}
			if firstErr == nil {
				if !reflect.DeepEqual(first, second) {
					t.Fatalf("O%d non-deterministic optimizer result", level)
				}
				fixed, err := Apply(first, level)
				if err != nil || !reflect.DeepEqual(first, fixed) {
					t.Fatalf("O%d optimizer is not idempotent: %v", level, err)
				}
			}
		}
		if !reflect.DeepEqual(program.Functions[0].Body, original) {
			t.Fatal("optimizer mutated fuzz input")
		}
	})
}
