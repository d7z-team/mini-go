package semantic

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestOperatorMethodNames(t *testing.T) {
	tests := []struct {
		operator string
		unary    bool
		method   string
	}{
		{operator: "+", method: "OpAdd"},
		{operator: "-", method: "OpSub"},
		{operator: "*", method: "OpMul"},
		{operator: "/", method: "OpDiv"},
		{operator: "%", method: "OpMod"},
		{operator: "&", method: "OpBitAnd"},
		{operator: "|", method: "OpBitOr"},
		{operator: "^", method: "OpBitXor"},
		{operator: "&^", method: "OpBitClear"},
		{operator: "<<", method: "OpLsh"},
		{operator: ">>", method: "OpRsh"},
		{operator: "==", method: "OpEq"},
		{operator: "!=", method: "OpNeq"},
		{operator: "<", method: "OpLt"},
		{operator: "<=", method: "OpLe"},
		{operator: ">", method: "OpGt"},
		{operator: ">=", method: "OpGe"},
		{operator: "+", unary: true, method: "OpPos"},
		{operator: "-", unary: true, method: "OpNeg"},
		{operator: "!", unary: true, method: "OpNot"},
		{operator: "^", unary: true, method: "OpBitNot"},
	}
	for _, tt := range tests {
		method, ok := operatorMethod(tt.operator, tt.unary)
		if !ok || method != tt.method {
			t.Fatalf("operatorMethod(%q, %v) = %q, %v; want %q", tt.operator, tt.unary, method, ok, tt.method)
		}
	}
	for _, operator := range []string{"&&", "||", "++", "--"} {
		if method, ok := operatorMethod(operator, false); ok {
			t.Fatalf("operatorMethod(%q) = %q, want unsupported", operator, method)
		}
	}
}

func TestAnalyzeResolvesOperatorMethods(t *testing.T) {
	parsed := parser.ParseSource("example/operators", "operators.mgo", `package operators

type Value struct { Number int; Data []int }

func (v Value) OpAdd(other Value) Value { return Value{Number: v.Number + other.Number} }
func (v Value) OpNeg() Value { return Value{Number: -v.Number} }
func (v Value) OpEq(other Value) bool { return v.Number == other.Number }

func Apply(left Value, right Value) (Value, Value, bool) {
	return left + right, -left, left == right
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse: %#v", parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatalf("analyze: %#v", checked.Info.Diagnostics)
	}
	var apply ast.FuncDecl
	for _, decl := range checked.Program.Files[0].Decls {
		if decl.Kind == ast.DeclFunc && decl.Func.Name == "Apply" {
			apply = *decl.Func
		}
	}
	want := []string{"OpAdd", "OpNeg", "OpEq"}
	for i, expression := range apply.Body.Stmts[0].Results {
		selection, ok := checked.Info.Operators[expression.NodeID]
		if !ok || selection.Selection.Name != want[i] {
			t.Fatalf("operator %d = %#v", i, selection)
		}
		info := checked.Info.Exprs[expression.NodeID]
		if i == 2 && !primitiveIs(checked.Info.Relations.View(info.Type), types.PrimitiveBool) {
			t.Fatalf("comparison result = %#v", info)
		}
	}
}

func TestAnalyzePrefersBuiltinOperatorAndRejectsInvalidOverload(t *testing.T) {
	parsed := parser.ParseSource("example/operators", "operators.mgo", `package operators

type Number int
func (n Number) OpAdd(other Number) Number { return 0 }

type Value struct { Data []int }
func (v Value) OpAdd() Value { return v }

func Apply(a Number, b Number, x Value) {
	_ = a + b
	_ = x + x
}
`)
	checked := Check(parsed.Program)
	var apply ast.FuncDecl
	for _, decl := range checked.Program.Files[0].Decls {
		if decl.Kind == ast.DeclFunc && decl.Func.Name == "Apply" {
			apply = *decl.Func
		}
	}
	if _, overloaded := checked.Info.Operators[apply.Body.Stmts[0].Right[0].NodeID]; overloaded {
		t.Fatal("builtin operation was replaced by an overload")
	}
	found := false
	for _, diagnostic := range checked.Info.Diagnostics {
		found = found || diagnostic.Code == "semantic.operator.signature"
	}
	if !found {
		t.Fatalf("missing overload signature diagnostic: %#v", checked.Info.Diagnostics)
	}
}

func TestAnalyzeResolvesOperatorMethodsAcrossTypeForms(t *testing.T) {
	parsed := parser.ParseSource("example/operators", "operators.mgo", `package operators

type Value struct { Number int; Data []int }
func (v Value) OpAdd(other Value) Value { return v }

type Wrapper struct { Value }
type Addable interface { OpAdd(Value) Value }

func FromInterface(left Addable, right Value) Value { return left + right }
func FromEmbedding(left Wrapper, right Value) Value { return left + right }
func Combine[T interface { OpAdd(T) T }](left T, right T) T { return left + right }
func Nested(left Value, middle Value, right Value) Value { return left + middle + right }
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse: %#v", parsed.Diagnostics)
	}
	checked := Check(parsed.Program)
	if len(checked.Info.Diagnostics) != 0 {
		t.Fatalf("analyze: %#v", checked.Info.Diagnostics)
	}
	if len(checked.Info.Operators) != 5 {
		t.Fatalf("operator selections = %#v", checked.Info.Operators)
	}
	for _, selection := range checked.Info.Operators {
		if selection.Selection.Name != "OpAdd" {
			t.Fatalf("operator selection = %#v", selection)
		}
	}
}

func TestAnalyzeRejectsInvalidOperatorResultAndReceiver(t *testing.T) {
	parsed := parser.ParseSource("example/operators", "operators.mgo", `package operators

type Value struct { Data []int }
func (v Value) OpEq(other Value) Value { return v }
func (v *Value) OpAdd(other Value) Value { return *v }
func Make() Value { return Value{} }

func Apply(left Value, right Value) {
	_ = left == right
	_ = Make() + right
}
`)
	checked := Check(parsed.Program)
	count := 0
	for _, diagnostic := range checked.Info.Diagnostics {
		if diagnostic.Code == "semantic.operator.signature" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("operator diagnostics = %#v", checked.Info.Diagnostics)
	}
}
