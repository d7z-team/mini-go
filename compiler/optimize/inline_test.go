package optimize

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestFullOptimizationInlinesSmallPureFunction(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	parameter := hir.Expression{Kind: hir.ExprLocal, Local: "value", Type: integer}
	one := hir.Expression{Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("1")}
	program := hir.Program{Functions: []hir.Function{
		{ID: "increment", Signature: types.FunctionSignature{Params: []types.TypeParam{{Type: integer}}, Results: []types.TypeRef{integer}}, Locals: []hir.Local{{ID: "value", Type: integer}}, Body: []hir.Statement{{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprBinary, Type: integer, Operator: "+", Left: &parameter, Right: &one}}}}},
		{ID: "main", Signature: types.FunctionSignature{Results: []types.TypeRef{integer}}, Body: []hir.Statement{{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprCallDirect, Function: "increment", ResultCount: 1, Args: []hir.Expression{{Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("41")}}}}}}},
	}}
	optimized, err := Apply(program, LevelFull)
	if err != nil {
		t.Fatal(err)
	}
	result := optimized.Functions[1].Body[0].Results[0]
	if result.Kind != hir.ExprBinary || result.Left == nil || result.Left.Kind != hir.ExprLiteral {
		t.Fatalf("small pure call was not inlined: %#v", result)
	}
}

func TestFullOptimizationKeepsCallsWithRepeatedArgumentsOrPanickingBodies(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	parameter := hir.Expression{Kind: hir.ExprLocal, Local: "value", Type: integer}
	program := hir.Program{Functions: []hir.Function{
		{ID: "repeat", Signature: types.FunctionSignature{Params: []types.TypeParam{{Type: integer}}, Results: []types.TypeRef{integer}}, Locals: []hir.Local{{ID: "value", Type: integer}}, Body: []hir.Statement{{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprBinary, Type: integer, Operator: "+", Left: &parameter, Right: &parameter}}}}},
		{ID: "divide", Signature: types.FunctionSignature{Params: []types.TypeParam{{Type: integer}}, Results: []types.TypeRef{integer}}, Locals: []hir.Local{{ID: "value", Type: integer}}, Body: []hir.Statement{{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprBinary, Type: integer, Operator: "/", Left: &parameter, Right: &parameter}}}}},
	}}
	optimized := inlineSmallPureCalls(program.Functions)
	if len(optimized) != 2 || optimized[0].Body[0].Results[0].Kind != hir.ExprBinary || optimized[1].Body[0].Results[0].Kind != hir.ExprBinary {
		t.Fatalf("candidate filtering changed function bodies: %#v", optimized)
	}
}
