package semantic

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestCheckReportsMissingFunctionExpression(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", "package main\nvar callback = func() {}\n")
	if len(parsed.Diagnostics) != 0 {
		t.Fatal(parsed.Diagnostics)
	}
	parsed.Program.Files[0].Decls[0].Var.Values[0].Func = nil
	checked := Check(parsed.Program)
	for _, diagnostic := range checked.Info.Diagnostics {
		if diagnostic.Code == "ast.expr.func.missing" {
			return
		}
	}
	t.Fatalf("missing function diagnostic: %+v", checked.Info.Diagnostics)
}

func TestAnalyzeResolvesPackageFunctionAndBlockScopes(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

var PackageValue, Other int

func F(param int) (result int) {
	local := PackageValue
	{
		local := param
		result = local
	}
	return local
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}

	packageDecl := parsed.Program.Files[0].Decls[0]
	if got := len(info.Defs[packageDecl.NodeID]); got != 2 {
		t.Fatalf("package declaration definitions = %d, want 2", got)
	}
	packageValue, ok := info.Lookup(info.PackageScope, "PackageValue")
	if !ok || packageValue.Kind != ObjectVar {
		t.Fatalf("missing package variable: %#v", packageValue)
	}

	function := parsed.Program.Files[0].Decls[1].Func
	functionScope := info.NodeScopes[function.NodeID]
	param, ok := info.Lookup(functionScope, "param")
	if !ok || param.Scope != functionScope {
		t.Fatalf("missing function parameter: %#v", param)
	}
	outerShort := function.Body.Stmts[0]
	outerLocalID := info.Defs[outerShort.Left[0].NodeID][0]
	if got := info.Uses[outerShort.Right[0].NodeID]; got != packageValue.ID {
		t.Fatalf("package use resolved to %q, want %q", got, packageValue.ID)
	}

	innerBlock := function.Body.Stmts[1]
	innerShort := innerBlock.Body.Stmts[0]
	innerLocalID := info.Defs[innerShort.Left[0].NodeID][0]
	if innerLocalID == outerLocalID {
		t.Fatalf("nested short declaration reused outer object %q", outerLocalID)
	}
	if got := info.Uses[innerShort.Right[0].NodeID]; got != param.ID {
		t.Fatalf("parameter use resolved to %q, want %q", got, param.ID)
	}
	innerAssign := innerBlock.Body.Stmts[1]
	if got := info.Uses[innerAssign.Right[0].NodeID]; got != innerLocalID {
		t.Fatalf("inner local use resolved to %q, want %q", got, innerLocalID)
	}
	outerReturn := function.Body.Stmts[2]
	if got := info.Uses[outerReturn.Results[0].NodeID]; got != outerLocalID {
		t.Fatalf("outer local use resolved to %q, want %q", got, outerLocalID)
	}
	if scope := info.Scope(info.NodeScopes[innerBlock.Body.NodeID]); scope == nil || scope.Kind != ScopeBlock || scope.Parent != functionScope {
		t.Fatalf("unexpected nested block scope: %#v", scope)
	}
}

func TestAnalyzePredeclaredAliasesUseCanonicalNumericTypes(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main
var b byte
var r rune
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	byteObject, ok := info.Lookup(info.Universe, "byte")
	if !ok || byteObject.Type != types.Builtin(types.PrimitiveUint8) {
		t.Fatalf("byte object = %#v", byteObject)
	}
	runeObject, ok := info.Lookup(info.Universe, "rune")
	if !ok || runeObject.Type != types.Builtin(types.PrimitiveInt32) {
		t.Fatalf("rune object = %#v", runeObject)
	}
	b, ok := info.Lookup(info.PackageScope, "b")
	if !ok || b.Type != types.Builtin(types.PrimitiveUint8) {
		t.Fatalf("byte declaration = %#v", b)
	}
	r, ok := info.Lookup(info.PackageScope, "r")
	if !ok || r.Type != types.Builtin(types.PrimitiveInt32) {
		t.Fatalf("rune declaration = %#v", r)
	}
}

func TestAnalyzeInfersRuneConstantsAndForwardPackageVariables(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

const RuneValue = '世'
var Public = private
var private = []int{1, 2, 3}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}
	runeValue, ok := info.Lookup(info.PackageScope, "RuneValue")
	if !ok || runeValue.Type != types.Builtin(types.PrimitiveInt32) || !runeValue.Untyped {
		t.Fatalf("rune constant = %#v", runeValue)
	}
	public, ok := info.Lookup(info.PackageScope, "Public")
	if !ok {
		t.Fatal("Public variable is missing")
	}
	if shape := info.Relations.View(public.Type).Shape(); shape != types.Slice {
		t.Fatalf("Public type = %s", types.FormatWithTable(info.TypeTable, public.Type))
	}
}

func TestAnalyzeInfersForwardPackageVariableGroups(t *testing.T) {
	first := parser.ParseSource("example/main", "first.mgo", `package main

var EarlyLeft = left
var EarlyRight = right
`)
	second := parser.ParseSource("example/main", "second.mgo", `package main

var left, right = pair()

func pair() (int64, string) { return 1, "one" }
`)
	if len(first.Diagnostics) != 0 || len(second.Diagnostics) != 0 {
		t.Fatalf("parse source: first=%#v second=%#v", first.Diagnostics, second.Diagnostics)
	}
	program := first.Program
	program.Files = append(program.Files, second.Program.Files...)
	info := Check(program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}
	for name, want := range map[string]types.TypeRef{
		"EarlyLeft":  types.Builtin(types.PrimitiveInt64),
		"left":       types.Builtin(types.PrimitiveInt64),
		"EarlyRight": types.Builtin(types.PrimitiveString),
		"right":      types.Builtin(types.PrimitiveString),
	} {
		object, ok := info.Lookup(info.PackageScope, name)
		if !ok || object.Type != want {
			t.Fatalf("%s type = %#v, want %#v", name, object.Type, want)
		}
	}
}

func TestAnalyzeRejectsPackageVariableGroupCycle(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

var Use = first
var first, _ = 1, first
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	found := false
	for _, diagnostic := range info.Diagnostics {
		if diagnostic.Code == "semantic.var.cycle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %#v, want semantic.var.cycle", info.Diagnostics)
	}
}

func TestAnalyzeRejectsDeclarationValueCountMismatch(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

var first, second = 1
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 1 || info.Diagnostics[0].Code != "semantic.decl.value_count" {
		t.Fatalf("diagnostics = %#v, want semantic.decl.value_count", info.Diagnostics)
	}
}

func TestAnalyzeClassifiesGenericDeclarationsAndExplicitInstantiation(t *testing.T) {
	parsed := parser.ParseSource("example/generic", "generic.mgo", `package generic

type Number interface { ~int | ~int64 }
type Pair[A, B any] struct { First A; Second B }

func Identity[T Number](value T) T { return value }
func Use(value int) int { return Identity[int](value) }
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	decls := parsed.Program.Files[0].Decls
	identityObject, ok := info.Lookup(info.PackageScope, "Identity")
	if !ok || len(info.GenericDecls[identityObject.ID]) != 1 {
		t.Fatalf("generic Identity metadata = %#v", info.GenericDecls[identityObject.ID])
	}
	identity := decls[2].Func
	paramType := info.Types[identity.Params[0].Type.NodeID].Type
	if paramType.Kind != types.TypeParameter {
		t.Fatalf("Identity parameter type = %#v, want type parameter", paramType)
	}
	call := decls[3].Func.Body.Stmts[0].Results[0]
	instance, ok := info.Instances[call.Callee.NodeID]
	if !ok || instance.Generic != identityObject.ID || len(instance.TypeArgs) != 1 || instance.TypeArgs[0] != types.Builtin(types.PrimitiveInt) {
		t.Fatalf("explicit instantiation = %#v", instance)
	}
	callInfo, ok := info.Calls[call.NodeID]
	if !ok || callInfo.Callee != identityObject.ID || len(callInfo.TypeArgs) != 1 {
		t.Fatalf("generic call metadata = %#v", callInfo)
	}
}

func TestAnalyzeBuildsStructuredCompositeAndConstraintTypes(t *testing.T) {
	parsed := parser.ParseSource("example/types", "types.mgo", `package types
type Number interface { ~int | ~int64 }
type Record struct { Values []int; Index map[string]*int }
func Apply(fn func(int) string, input <-chan int) string { return fn(<-input) }
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if err := info.TypeTable.ValidateCompiler(); err != nil {
		t.Fatalf("validate semantic type table: %v", err)
	}
	number, ok := info.Lookup(info.PackageScope, "Number")
	if !ok {
		t.Fatal("missing Number type")
	}
	methods, terms, typeSet, ok := info.Relations.View(number.Type).Interface()
	if !ok || !typeSet || len(methods) != 0 || len(terms) != 2 || !terms[0].Approx || !terms[1].Approx {
		t.Fatalf("unexpected Number constraint: methods=%#v terms=%#v typeSet=%v", methods, terms, typeSet)
	}
	record, ok := info.Lookup(info.PackageScope, "Record")
	if !ok {
		t.Fatal("missing Record type")
	}
	fields, ok := info.Relations.View(record.Type).StructFields()
	if !ok || len(fields) != 2 || info.Relations.View(fields[0].Type).Shape() != types.Slice || info.Relations.View(fields[1].Type).Shape() != types.Map {
		t.Fatalf("unexpected Record fields: %#v", fields)
	}
	apply, ok := info.Lookup(info.PackageScope, "Apply")
	if !ok {
		t.Fatal("missing Apply function")
	}
	signature, ok := info.Relations.View(apply.Type).Function()
	if !ok || len(signature.Params) != 2 || info.Relations.View(signature.Params[0].Type).Shape() != types.Function {
		t.Fatalf("unexpected Apply signature: %#v", signature)
	}
	direction, element, ok := info.Relations.View(signature.Params[1].Type).Waitable()
	if !ok || direction != types.ChannelReceive || element != types.Builtin(types.PrimitiveInt) {
		t.Fatalf("unexpected receive-only parameter: direction=%v element=%#v", direction, element)
	}
}

func TestAnalyzeBuildsFunctionCallFactsBeforeBodies(t *testing.T) {
	parsed := parser.ParseSource("example/functions", "functions.mgo", `package functions

func Direct(value int64) int64 { return Later(value) }
func Local(value int64) int64 { fn := Later; return fn(value) }
func Returned(value int64) int64 { fn := Make(); return fn(value) }
func Multi() (int64, string) { return Pair() }
func Empty() { Sink() }

func Later(value int64) int64 { return value + 1 }
func Make() func(int64) int64 { return Later }
func Pair() (int64, string) { return 1, "one" }
func Sink() {}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}

	functions := map[string]ast.FuncDecl{}
	for _, decl := range parsed.Program.Files[0].Decls {
		if decl.Kind == ast.DeclFunc {
			functions[decl.Func.Name] = *decl.Func
		}
	}
	directCall := functions["Direct"].Body.Stmts[0].Results[0]
	direct := info.Calls[directCall.NodeID]
	if len(direct.Signature.Params) != 1 || len(direct.Signature.Results) != 1 || direct.Signature.Results[0] != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("forward call facts = %#v", direct)
	}

	localFunction := functions["Local"]
	localCall := localFunction.Body.Stmts[1].Results[0]
	local := info.Calls[localCall.NodeID]
	if len(local.Signature.Results) != 1 || local.Signature.Results[0] != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("local function value call facts = %#v", local)
	}
	localObject := info.Exprs[localCall.Callee.NodeID]
	if localObject.Signature == nil || localObject.Object == "" {
		t.Fatalf("local function value facts = %#v", localObject)
	}

	returnedFunction := functions["Returned"]
	makeCall := returnedFunction.Body.Stmts[0].Right[0]
	returnedCall := returnedFunction.Body.Stmts[1].Results[0]
	if got := info.Exprs[makeCall.NodeID]; got.Mode != ExprValue || !got.Type.Valid() || got.Signature == nil {
		t.Fatalf("function result facts = %#v", got)
	}
	if got := info.Calls[returnedCall.NodeID]; len(got.Signature.Results) != 1 || got.Signature.Results[0] != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("returned function call facts = %#v", got)
	}

	multiCall := functions["Multi"].Body.Stmts[0].Results[0]
	if got := info.Exprs[multiCall.NodeID]; got.Mode != ExprMultiValue || len(got.Results) != 2 {
		t.Fatalf("multi-result call facts = %#v", got)
	}
	emptyCall := functions["Empty"].Body.Stmts[0].Expr
	if emptyCall == nil {
		t.Fatal("missing empty call expression")
	}
	if got := info.Exprs[emptyCall.NodeID]; got.Mode != ExprNoValue || len(got.Results) != 0 {
		t.Fatalf("no-result call facts = %#v", got)
	}
	if err := info.TypeTable.ValidateCompiler(); err != nil {
		t.Fatalf("validate semantic type table: %v", err)
	}
}

func TestAnalyzeReportsInvalidDependencyMetadata(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", "package main\n")
	checked := WithOptions(parsed.Program, AnalyzeOptions{Dependencies: testDependencies([]DependencyExport{{
		ModulePath: "example/lib",
		Name:       "Value",
	}})})
	if len(checked.Info.Diagnostics) != 1 || checked.Info.Diagnostics[0].Code != "semantic.dependency.kind" {
		t.Fatalf("invalid dependency diagnostics = %#v", checked.Info.Diagnostics)
	}
}
