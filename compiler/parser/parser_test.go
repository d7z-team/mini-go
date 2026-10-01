package parser

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func TestParseSourceDeclarationsAndTypes(t *testing.T) {
	source := `package main

import (
	"fmt"
	alias "example/lib"
)

const (
	Answer int = 42
	Name = "mini"
)

var numbers []int = []int{1, 2, 3}

type Point struct {
	X, Y int ` + "`json:\"x\"`" + `
	*alias.Node
}

type Reader interface {
	Read([]byte) (int, error)
	fmt.Stringer
	~int | ~string
}

func (p *Point) Sum(scale int) int {
	return (p.X + p.Y) * scale
}
`
	result := ParseSource("example/main", "main.mgo", source)
	requireNoDiagnostics(t, result)
	program := result.Program
	if program.ModulePath != "example/main" || program.Package != "main" {
		t.Fatalf("unexpected program header: %+v", program)
	}
	if len(program.Files) != 1 {
		t.Fatalf("file count = %d, want 1", len(program.Files))
	}
	decls := program.Files[0].Decls
	if len(decls) != 8 {
		t.Fatalf("decl count = %d, want 8", len(decls))
	}
	point := findDecl(t, decls, ast.DeclType, "Point")
	if point.Type.Type.Kind != ast.TypeStruct || len(point.Type.Type.Fields) != 3 {
		t.Fatalf("unexpected Point type: %+v", point.Type.Type)
	}
	reader := findDecl(t, decls, ast.DeclType, "Reader")
	if reader.Type.Type.Kind != ast.TypeInterface || len(reader.Type.Type.Methods) != 1 || len(reader.Type.Type.Embeds) != 1 || len(reader.Type.Type.Terms) != 2 {
		t.Fatalf("unexpected Reader interface: %+v", reader.Type.Type)
	}
	method := findDecl(t, decls, ast.DeclFunc, "Sum")
	if method.Func.Receiver == nil || method.Func.Receiver.Type.Kind != ast.TypePointer {
		t.Fatalf("expected pointer receiver, got %+v", method.Func.Receiver)
	}
	if got := method.Func.Results[0].Type.Name; got != "int" {
		t.Fatalf("method result canonical = %q, want Int", got)
	}
}

func TestParseSourceStatementsAndExpressions(t *testing.T) {
	source := `package main

func Run(ch chan int, anyValue any) int {
	total := 0
	values := []int{1, 2: 3}
	for i, v := range values {
		total += i + v
	}
	if total > 0 {
		defer func(x int) {
			total += x
		}(1)
	} else {
		go func() {
			total++
		}()
	}
	switch x := anyValue.(type) {
	case nil:
		total = 0
	case int, string:
		_ = x
	default:
		total = total + 1
	}
	select {
	case ch <- total:
	default:
		total--
	}
	return total
}
`
	result := ParseSource("example/run", "run.mgo", source)
	requireNoDiagnostics(t, result)
	run := findDecl(t, result.Program.Files[0].Decls, ast.DeclFunc, "Run")
	body := run.Func.Body.Stmts
	requireStmtKind(t, body, ast.StmtAssign)
	requireStmtKind(t, body, ast.StmtRange)
	requireStmtKind(t, body, ast.StmtIf)
	requireStmtKind(t, body, ast.StmtSwitch)
	requireStmtKind(t, body, ast.StmtSelect)
	requireStmtKind(t, body, ast.StmtReturn)
	if body[2].Kind != ast.StmtRange || body[2].Op != ":=" || body[2].Range == nil {
		t.Fatalf("unexpected range statement: %+v", body[2])
	}
	if !body[4].TypeSwitch || body[4].TypeSwitchName != "x" {
		t.Fatalf("expected named type switch, got %+v", body[4])
	}
	if len(body[5].Cases) != 2 || body[5].Cases[0].Comm == nil || body[5].Cases[0].Comm.Kind != ast.StmtSend {
		t.Fatalf("unexpected select cases: %+v", body[5].Cases)
	}
}

func TestParseSourceRejectsInvalidTypeSwitchGuards(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{
			name: "assignment",
			src: `package main
func Main(value any) {
	var out any
	switch out = value.(type) { case int: _ = out }
}`,
			code: "parser.typeswitch.guard.assign",
		},
		{
			name: "multiple binding",
			src: `package main
func Main(value any) {
	switch a, b := value.(type) { case int: _, _ = a, b }
}`,
			code: "parser.typeswitch.guard.binding",
		},
		{
			name: "selector binding",
			src: `package main
type box struct { value any }
func Main(value any) {
	var target box
	switch target.value := value.(type) { case int: _ = target }
}`,
			code: "parser.typeswitch.guard.binding",
		},
		{
			name: "blank binding",
			src: `package main
func Main(value any) {
	switch _ := value.(type) { case int: }
}`,
			code: "parser.typeswitch.guard.blank",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := ParseSource("example/typeswitch", tc.name+".mgo", tc.src)
			requireDiagnostic(t, result, tc.code)
		})
	}
}

func TestParseSourceFunctionLiteralAndComposite(t *testing.T) {
	source := `package main

var F = func(values map[string][]int) int {
	return values["a"][0:1:1][0]
}
`
	result := ParseSource("example/lit", "lit.mgo", source)
	requireNoDiagnostics(t, result)
	decl := result.Program.Files[0].Decls[0]
	if decl.Kind != ast.DeclVar || len(decl.Var.Values) != 1 {
		t.Fatalf("unexpected var decl: %+v", decl)
	}
	value := decl.Var.Values[0]
	if value.Kind != ast.ExprFunc || len(value.Func.Params) != 1 {
		t.Fatalf("expected function literal, got %+v", value)
	}
	if value.Func.Params[0].Type.Kind != ast.TypeMap {
		t.Fatalf("expected map parameter, got %+v", value.Func.Params[0].Type)
	}
}

func TestParseTypeLiteralConversions(t *testing.T) {
	source := `package main

func Convert(value any, fn any) {
	_ = interface{ String() string }(value)
	_ = func(int) int(fn)
	_ = func(value int) int { return value }(1)
}
`
	result := ParseSource("example/conversion", "conversion.mgo", source)
	requireNoDiagnostics(t, result)
	body := findDecl(t, result.Program.Files[0].Decls, ast.DeclFunc, "Convert").Func.Body.Stmts
	for i := 0; i < 2; i++ {
		if body[i].Right[0].Kind != ast.ExprConvert {
			t.Fatalf("statement %d expression kind = %s, want conversion", i, body[i].Right[0].Kind)
		}
	}
	if body[2].Right[0].Kind != ast.ExprCall || body[2].Right[0].Callee.Kind != ast.ExprFunc {
		t.Fatalf("function literal call was not preserved: %+v", body[2].Right[0])
	}
}

func TestParseInstantiatedFieldsAndTypeParameters(t *testing.T) {
	source := `package main

type Box[T any] struct { Value T }
type Pair[T any] struct {
	Box[T]
	*Box[T]
}

func Apply[_ any, P (interface{ ~int | ~int8 }),](Box[int], P) {}
`
	result := ParseSource("example/generic-fields", "generic_fields.mgo", source)
	requireNoDiagnostics(t, result)
	pair := findDecl(t, result.Program.Files[0].Decls, ast.DeclType, "Pair")
	if len(pair.Type.Type.Fields) != 2 || pair.Type.Type.Fields[0].Type.Kind != ast.TypeInstance {
		t.Fatalf("unexpected instantiated embedded fields: %+v", pair.Type.Type.Fields)
	}
	apply := findDecl(t, result.Program.Files[0].Decls, ast.DeclFunc, "Apply")
	if len(apply.Func.TypeParams) != 2 || apply.Func.TypeParams[0].Name != "_" {
		t.Fatalf("unexpected type parameters: %+v", apply.Func.TypeParams)
	}
	if len(apply.Func.Params) != 2 || apply.Func.Params[0].Type.Kind != ast.TypeInstance {
		t.Fatalf("unexpected unnamed parameters: %+v", apply.Func.Params)
	}
}

func TestParseSourceFunctionTypeUnnamedParameterList(t *testing.T) {
	source := `package main

type Span struct{}
type Pair struct{}
type Callback func(string, string, input.Span)
type PairCallback func(Pair, Pair)

func Named(a, b int, cb func(string, string, input.Span)) {}
`
	result := ParseSource("example/sig", "sig.mgo", source)
	requireNoDiagnostics(t, result)
	decls := result.Program.Files[0].Decls
	callback := findDecl(t, decls, ast.DeclType, "Callback")
	if len(callback.Type.Type.Params) != 3 {
		t.Fatalf("Callback param count = %d, want 3: %+v", len(callback.Type.Type.Params), callback.Type.Type.Params)
	}
	if callback.Type.Type.Params[0].Name != "" || callback.Type.Type.Params[0].Type.Name != "string" {
		t.Fatalf("unexpected first Callback param: %+v", callback.Type.Type.Params[0])
	}
	if callback.Type.Type.Params[2].Type.Name != "input.Span" {
		t.Fatalf("unexpected selector Callback param: %+v", callback.Type.Type.Params[2])
	}
	pairCallback := findDecl(t, decls, ast.DeclType, "PairCallback")
	if len(pairCallback.Type.Type.Params) != 2 || pairCallback.Type.Type.Params[0].Name != "" || pairCallback.Type.Type.Params[0].Type.Name != "Pair" {
		t.Fatalf("unexpected PairCallback params: %+v", pairCallback.Type.Type.Params)
	}
	named := findDecl(t, decls, ast.DeclFunc, "Named")
	if len(named.Func.Params) != 3 || named.Func.Params[0].Name != "a" || named.Func.Params[1].Name != "b" {
		t.Fatalf("named parameter group was not preserved: %+v", named.Func.Params)
	}
	if named.Func.Params[2].Type.Kind != ast.TypeFunc || len(named.Func.Params[2].Type.Params) != 3 {
		t.Fatalf("unexpected function-typed parameter: %+v", named.Func.Params[2])
	}
}

func TestParseSourcePanicStatement(t *testing.T) {
	source := `package main

func Run() {
	panic("boom")
}
`
	result := ParseSource("example/panic", "panic.mgo", source)
	requireNoDiagnostics(t, result)
	run := findDecl(t, result.Program.Files[0].Decls, ast.DeclFunc, "Run")
	if len(run.Func.Body.Stmts) != 1 || run.Func.Body.Stmts[0].Kind != ast.StmtPanic {
		t.Fatalf("expected panic statement, got %+v", run.Func.Body.Stmts)
	}
	if run.Func.Body.Stmts[0].Expr == nil || run.Func.Body.Stmts[0].Expr.Literal != `"boom"` {
		t.Fatalf("unexpected panic expression: %+v", run.Func.Body.Stmts[0].Expr)
	}
}

func TestInheritedConstantCallsKeepIndependentIotaAndTypes(t *testing.T) {
	result := ParseSource("sample", "main.mgo", "package sample\nconst ( A = max(iota, 0); B; C )\n")
	requireNoDiagnostics(t, result)
	if diagnostics, _ := ast.FinalizeStructure(&result.Program, ast.Limits{}); len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	for index, decl := range result.Program.Files[0].Decls {
		call := decl.Const.Values[0]
		if call.Args[0].Literal != []string{"0", "1", "2"}[index] {
			t.Fatalf("constant %d inherited rewritten arguments: %+v", index, call.Args)
		}
	}
	a := &result.Program.Files[0].Decls[0].Const.Values[0].Args[1]
	b := &result.Program.Files[0].Decls[1].Const.Values[0].Args[1]
	if a.Type.NodeID == b.Type.NodeID {
		t.Fatal("independent constant occurrences share type identity")
	}
	b.Type.Name = "String"
	if a.Type.Name != "Int" {
		t.Fatal("editing one constant changed another constant type")
	}
}

func TestParseSourceConstIotaLiteralAndBuiltinTypeArgs(t *testing.T) {
	source := `package main

const (
	A = iota
	B
	C = iota + 10
)

func Run() {
	_ = "a\x62"
	_ = '\u754c'
	_ = 0x2a
	_ = 1.25e2
	_ = 3i
	_ = make([]byte, 2)
	_ = new(map[string]int)
	v := 1
	_ = new(v)
}

`
	result := ParseSource("example/literals", "literals.mgo", source)
	requireNoDiagnostics(t, result)
	decls := result.Program.Files[0].Decls
	if decls[0].Const.Values[0].Literal != "0" || decls[1].Const.Values[0].Literal != "1" {
		t.Fatalf("unexpected iota values: %+v %+v", decls[0].Const.Values, decls[1].Const.Values)
	}
	if decls[2].Const.Values[0].Right == nil || decls[2].Const.Values[0].Right.Literal != "10" {
		t.Fatalf("unexpected explicit iota expression: %+v", decls[2].Const.Values)
	}
	run := findDecl(t, decls, ast.DeclFunc, "Run")
	body := run.Func.Body.Stmts
	if body[0].Right[0].Literal != `"ab"` || body[1].Right[0].Literal != "30028" || body[2].Right[0].Literal != "0x2a" {
		t.Fatalf("unexpected normalized literals: %+v", body[:3])
	}
	makeCall := body[5].Right[0]
	if makeCall.Kind != ast.ExprCall || len(makeCall.Args) == 0 || makeCall.Args[0].Type.Kind != ast.TypeSlice {
		t.Fatalf("expected make type argument, got %+v", makeCall)
	}
	newCall := body[6].Right[0]
	if newCall.Kind != ast.ExprCall || len(newCall.Args) == 0 || newCall.Args[0].Type.Kind != ast.TypeMap {
		t.Fatalf("expected new type argument, got %+v", newCall)
	}
	newExprCall := body[8].Right[0]
	if newExprCall.Kind != ast.ExprCall || len(newExprCall.Args) == 0 || newExprCall.Args[0].Kind != ast.ExprIdent || newExprCall.Args[0].Name != "v" {
		t.Fatalf("expected new expression argument, got %+v", newExprCall)
	}
}

func TestParseSourceGenericDeclarationsAndInstantiations(t *testing.T) {
	result := ParseSource("example/generic", "generic.mgo", `package generic

type Number interface { ~int | ~int64 }
type Pair[A, B any] struct {
	First A
	Second B
}

func Identity[T Number](value T) T { return value }

func Use(value int) int {
	return Identity[int](value)
}
`)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", result.Diagnostics)
	}
	decls := result.Program.Files[0].Decls
	if len(decls) != 4 {
		t.Fatalf("declaration count = %d, want 4", len(decls))
	}
	pair := decls[1].Type
	if len(pair.TypeParams) != 2 || pair.TypeParams[0].Name != "A" || pair.TypeParams[1].Name != "B" {
		t.Fatalf("unexpected Pair type parameters: %#v", pair.TypeParams)
	}
	identity := decls[2].Func
	if len(identity.TypeParams) != 1 || identity.TypeParams[0].Name != "T" || identity.TypeParams[0].Constraint.Name != "Number" {
		t.Fatalf("unexpected Identity type parameters: %#v", identity.TypeParams)
	}
	call := decls[3].Func.Body.Stmts[0].Results[0]
	if call.Kind != ast.ExprCall || call.Callee == nil || call.Callee.Kind != ast.ExprIndex {
		t.Fatalf("single type argument must remain a neutral bracket expression: %#v", call)
	}
	if call.Callee.Index == nil || call.Callee.Index.Kind != ast.ExprIdent || call.Callee.Index.Name != "int" {
		t.Fatalf("unexpected explicit type argument: %#v", call.Callee.Index)
	}
}

func TestParseSourceParenthesizedTypeForms(t *testing.T) {
	source := `package main

func Run(value any, fn func(int) int) int {
	values := ([]int){1, 2}
	holder := (struct{ Values []int }){Values: values}
	made := make(([]int), 2)
	_ = new((map[string]int))
	_ = value.((interface{}))
	_ = (fn)(values[0])
	_ = (func(v int) int { return v })(1)
	switch value.(type) {
	case ([]int):
		_ = holder
	default:
	}
	return made[0]
}
`
	result := ParseSource("example/paren", "paren.mgo", source)
	requireNoDiagnostics(t, result)
	run := findDecl(t, result.Program.Files[0].Decls, ast.DeclFunc, "Run")
	body := run.Func.Body.Stmts
	if got := body[0].Right[0]; got.Kind != ast.ExprComposite || got.Type.Kind != ast.TypeSlice {
		t.Fatalf("expected parenthesized slice composite type, got %+v", got)
	}
	if got := body[1].Right[0]; got.Kind != ast.ExprComposite || got.Type.Kind != ast.TypeStruct {
		t.Fatalf("expected parenthesized struct composite type, got %+v", got)
	}
	if got := body[2].Right[0]; got.Kind != ast.ExprCall || len(got.Args) == 0 || got.Args[0].Type.Kind != ast.TypeSlice {
		t.Fatalf("expected parenthesized make slice type argument, got %+v", got)
	}
	if got := body[3].Right[0]; got.Kind != ast.ExprCall || len(got.Args) == 0 || got.Args[0].Type.Kind != ast.TypeMap {
		t.Fatalf("expected parenthesized new map type argument, got %+v", got)
	}
	if got := body[4].Right[0]; got.Kind != ast.ExprAssert || got.Type.Kind != ast.TypeInterface {
		t.Fatalf("expected parenthesized type assertion target, got %+v", got)
	}
	if got := body[5].Right[0]; got.Kind != ast.ExprCall || got.Callee == nil || got.Callee.Kind != ast.ExprIdent || got.Callee.Name != "fn" {
		t.Fatalf("expected grouped function value call to remain expression call, got %+v", got)
	}
	if got := body[6].Right[0]; got.Kind != ast.ExprCall || got.Callee == nil || got.Callee.Kind != ast.ExprFunc {
		t.Fatalf("expected grouped function literal call to remain expression call, got %+v", got)
	}
	switchStmt := body[7]
	if switchStmt.Kind != ast.StmtSwitch || !switchStmt.TypeSwitch || len(switchStmt.Cases) == 0 || len(switchStmt.Cases[0].Types) != 1 || switchStmt.Cases[0].Types[0].Kind != ast.TypeSlice {
		t.Fatalf("expected parenthesized type switch case target, got %+v", switchStmt)
	}
}

func TestDelimitedCompositeExpressionsInsideControlConditions(t *testing.T) {
	for _, statement := range []string{
		`if pair == (Pair{1, 2}) { return 1 }`,
		`for pair != ((Pair{1, 2})) { break }`,
		`switch (Pair{1, 2}) { case pair: return 1 }`,
		`if values[:Pair{1, 2}[0]] != nil { return 1 }`,
		`if values[0:Pair{1, 2}[0]:Pair{1, 2}[1]] != nil { return 1 }`,
		`if func() bool { if pair == (Pair{1, 2}) { return true }; return false }() { return 1 }`,
	} {
		t.Run(statement, func(t *testing.T) {
			parsed := ParseSource("conditions", "conditions.mgo", "package conditions\ntype Pair [2]int\nfunc Match(pair Pair, values []int) int { "+statement+"; return 0 }\n")
			requireNoDiagnostics(t, parsed)
		})
	}
}

func TestParseSourceControlAndCompositeShapesForLowering(t *testing.T) {
	source := `package main

type Pair [2]int

func Run(v any, values map[Pair]int, ch chan int) int {
Outer:
	for i, value := range []Pair{{1, 2}, {3, 4}} {
		_ = values[Pair{1, 2}]
		switch v.(type) {
		case string:
			return i + value[0]
		}
		select {
		case <-ch:
			break Outer
		default:
		}
	}
	return 0
}

`
	result := ParseSource("example/control", "control.mgo", source)
	requireNoDiagnostics(t, result)
	run := findDecl(t, result.Program.Files[0].Decls, ast.DeclFunc, "Run")
	label := run.Func.Body.Stmts[0]
	if label.Kind != ast.StmtLabel || len(label.Body.Stmts) != 1 || label.Body.Stmts[0].Kind != ast.StmtRange {
		t.Fatalf("expected label wrapping range, got %+v", label)
	}
	rangeStmt := label.Body.Stmts[0]
	if rangeStmt.Key == nil || rangeStmt.Value == nil {
		t.Fatalf("expected range key/value targets, got %+v", rangeStmt)
	}
	if rangeStmt.Range == nil || rangeStmt.Range.Kind != ast.ExprComposite || len(rangeStmt.Range.Items) != 2 {
		t.Fatalf("expected elided composite range expression, got %+v", rangeStmt.Range)
	}
	switchStmt := rangeStmt.Body.Stmts[1]
	if !switchStmt.TypeSwitch || switchStmt.Expr == nil || switchStmt.Expr.Kind != ast.ExprIdent || switchStmt.Expr.Name != "v" {
		t.Fatalf("expected type switch subject, got %+v", switchStmt)
	}
	selectStmt := rangeStmt.Body.Stmts[2]
	if selectStmt.Kind != ast.StmtSelect || selectStmt.Cases[0].Comm == nil || selectStmt.Cases[0].Comm.Kind != ast.StmtExpr || selectStmt.Cases[0].Comm.Expr.Kind != ast.ExprReceive {
		t.Fatalf("expected receive select comm, got %+v", selectStmt)
	}
}
