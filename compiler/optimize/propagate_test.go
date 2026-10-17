package optimize

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestPropagationInvalidatesCopiesWhenSourcesChange(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	a := hir.Expression{Kind: hir.ExprLocal, Local: "a", Type: integer}
	b := hir.Expression{Kind: hir.ExprLocal, Local: "b", Type: integer}
	for _, write := range []hir.Statement{
		{Kind: hir.StmtStoreLocal, Local: "a", Expr: hir.Expression{Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("9")}},
		{Kind: hir.StmtStoreValues, Targets: []hir.StoreTarget{{Kind: "local", Local: "a"}, {Kind: "local", Local: "c"}}, Values: []hir.Expression{b, a}},
	} {
		body := propagateLocalValues([]hir.Statement{
			{Kind: hir.StmtExpr},
			{Kind: hir.StmtStoreLocal, Local: "b", Expr: a},
			write,
			{Kind: hir.StmtReturn, Results: []hir.Expression{b}},
		}, &types.TypeTable{})
		if result := body[3].Results[0]; result.Kind != hir.ExprLocal || result.Local != "b" {
			t.Fatalf("copy changed after source write: %#v", result)
		}
	}
}

func TestPropagationDoesNotAliasCapturedCopySources(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	body := propagateLocalValues([]hir.Statement{
		{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprFunction, Captures: []hir.CaptureTarget{{Local: "a"}}}},
		{Kind: hir.StmtStoreLocal, Local: "b", Expr: hir.Expression{Kind: hir.ExprLocal, Local: "a", Type: integer}},
		{Kind: hir.StmtExpr, Expr: hir.Expression{Kind: hir.ExprCallValue}},
		{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprLocal, Local: "b", Type: integer}}},
	}, &types.TypeTable{})
	if result := body[3].Results[0]; result.Kind != hir.ExprLocal || result.Local != "b" {
		t.Fatalf("copy aliases captured source: %#v", result)
	}
}

func TestPropagationUsesLastAssignmentToRepeatedDestination(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	local := hir.Expression{Kind: hir.ExprLocal, Local: "value", Type: integer}
	literal := hir.Expression{Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("40")}
	sum := hir.Expression{Kind: hir.ExprBinary, Operator: "+", Type: integer, Left: &local, Right: &literal}
	body := propagateLocalValues([]hir.Statement{
		{
			Kind:    hir.StmtStoreValues,
			Targets: []hir.StoreTarget{{Kind: "local", Local: "value"}, {Kind: "local", Local: "value"}},
			Values:  []hir.Expression{literal, sum},
		},
		{Kind: hir.StmtReturn, Results: []hir.Expression{local}},
	}, &types.TypeTable{})
	if result := body[1].Results[0]; result.Kind != hir.ExprLocal || result.Local != "value" {
		t.Fatalf("propagation kept the overwritten value: %#v", result)
	}
}

func TestReplacementRespectsLetBindings(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	outer := hir.Expression{Kind: hir.ExprLocal, Local: "value", Type: integer}
	expression, changed := replaceKnownLocals(hir.Expression{
		Kind:  hir.ExprLet,
		Local: "value",
		Bind:  &hir.Expression{Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("8")},
		Body:  &outer,
	}, map[string]*hir.Expression{"value": {Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("7")}})
	if changed {
		t.Fatal("replacement crossed let binding")
	}
	letBody := expression.Body
	if letBody == nil || letBody.Kind != hir.ExprLocal || letBody.Local != "value" {
		t.Fatalf("propagation crossed let binding: %#v", letBody)
	}
}
