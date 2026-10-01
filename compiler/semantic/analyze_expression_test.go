package semantic

import (
	"fmt"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/types"
)

func TestAnalyzeBuildsOrdinaryExpressionTypeFacts(t *testing.T) {
	parsed := parser.ParseSource("example/expressions", "expressions.mgo", `package expressions

type Score int64

func Facts(xs []Score, ch chan int64) {
	mixed := 1 + int64(2)
	promoted := 1 + 2.5
	length := len(xs)
	appended := append(xs, Score(1))
	z := complex(float32(1), float32(2))
	realPart := real(z)
	element := xs[0]
	sliced := xs[:]
	received := <-ch
	_, _, _, _, _, _, _, _, _ = mixed, promoted, length, appended, z, realPart, element, sliced, received
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}

	statements := parsed.Program.Files[0].Decls[1].Func.Body.Stmts
	expression := func(index int) ExprInfo {
		return info.Exprs[statements[index].Right[0].NodeID]
	}
	mixed := expression(0)
	if mixed.Type != types.Builtin(types.PrimitiveInt64) || mixed.LeftTarget != mixed.Type || mixed.RightTarget != mixed.Type || mixed.Mode != ExprConstant || mixed.Untyped {
		t.Fatalf("mixed binary facts = %#v", mixed)
	}
	promoted := expression(1)
	if promoted.Type != types.Builtin(types.PrimitiveFloat64) || promoted.LeftTarget != promoted.Type || promoted.RightTarget != promoted.Type || promoted.Mode != ExprConstant || !promoted.Untyped {
		t.Fatalf("promoted binary facts = %#v", promoted)
	}
	if got := expression(2); got.Type != types.Builtin(types.PrimitiveInt) || got.Mode != ExprValue {
		t.Fatalf("len facts = %#v", got)
	}
	score, ok := info.Lookup(info.PackageScope, "Score")
	if !ok {
		t.Fatal("missing Score type")
	}
	appended := expression(3)
	if elem, ok := info.Relations.View(appended.Type).Elem(); !ok || info.Relations.View(appended.Type).Shape() != types.Slice || !info.Relations.Identical(elem, score.Type).OK {
		t.Fatalf("append facts = %#v", appended)
	}
	if got := expression(4); got.Type != types.Builtin(types.PrimitiveComplex64) || got.Untyped {
		t.Fatalf("complex facts = %#v", got)
	}
	if got := expression(5); got.Type != types.Builtin(types.PrimitiveFloat32) {
		t.Fatalf("real facts = %#v", got)
	}
	if got := expression(6); !info.Relations.Identical(got.Type, score.Type).OK || got.Category != ValueAddressable {
		t.Fatalf("index facts = %#v", got)
	}
	if got := expression(7); info.Relations.View(got.Type).Shape() != types.Slice {
		t.Fatalf("slice facts = %#v", got)
	}
	if got := expression(8); got.Type != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("receive facts = %#v", got)
	}
}

func TestAnalyzeReportsOrdinaryBinaryRelationErrors(t *testing.T) {
	parsed := parser.ParseSource("example/errors", "errors.mgo", `package errors

type First int
type Second int

func Invalid(first First, second Second, values []int) {
	_ = first + second
	_ = 1.5 % 1
	_ = values == values
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	codes := map[string]bool{}
	for _, diagnostic := range info.Diagnostics {
		codes[string(diagnostic.Code)] = true
	}
	for _, code := range []string{
		"hirgen.binary.numeric_types",
		"hirgen.binary.integer_operand",
		"hirgen.binary.comparable",
	} {
		if !codes[code] {
			t.Fatalf("missing diagnostic %q: %#v", code, info.Diagnostics)
		}
	}
}

func TestAnalyzeBuildsDependencyAndMethodSelectionFacts(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

import lib "example/lib"

type Local struct { Value int64 }
type Reader interface { Read(int64) (int64, bool) }

func (value Local) Get() int64 { return value.Value }
func (value *Local) Add(delta int64) int64 { value.Value += delta; return value.Value }

func Use(reader Reader, local Local) int64 {
	remote := lib.New(1)
	first, text := remote.Sum(2)
	get := local.Get
	add := local.Add
	getExpr := Local.Get
	addExpr := (*Local).Add
	read, ok := reader.Read(first)
	if ok && text != "" {
		return get() + add(1) + getExpr(local) + addExpr(&local, 1) + read + remote.Value
	}
	return 0
}

func Imported(reader lib.Reader) int64 {
	value, ok := reader.Read(1)
	if ok { return value }
	return 0
}

`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := WithOptions(parsed.Program, AnalyzeOptions{Dependencies: testDependencies([]DependencyExport{
		{ModulePath: "example/lib", Name: "New", ID: "fn.New", Kind: ObjectFunc, Type: "function(Int64) example/lib.Counter"},
		{
			ModulePath: "example/lib", Name: "Counter", ID: "type.Counter", Kind: ObjectType,
			Type: "example/lib.Counter", Underlying: "struct{Value:Int64}",
			Fields: []DependencyTypeField{{Name: "Value", Type: "Int64"}},
			Methods: []DependencyTypeMethod{{
				Name: "Sum", Receiver: "example/lib.Counter", Signature: "function(Int64) tuple(Int64, String)",
				FunctionID: "method.Counter.Sum", ModulePath: "example/lib",
			}},
		},
		{
			ModulePath: "example/lib", Name: "Reader", ID: "type.Reader", Kind: ObjectType,
			Type: "example/lib.Reader", Underlying: "interface{}",
			Methods: []DependencyTypeMethod{{
				Name: "Read", Receiver: "example/lib.Reader", Signature: "function(Int64) tuple(Int64, Bool)",
				ModulePath: "example/lib",
			}},
		},
	})}).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}

	functions := map[string]ast.FuncDecl{}
	for _, decl := range parsed.Program.Files[0].Decls {
		if decl.Kind == ast.DeclFunc {
			functions[decl.Func.Name] = *decl.Func
		}
	}
	use := functions["Use"]
	newCall := use.Body.Stmts[0].Right[0]
	if call := info.Calls[newCall.NodeID]; call.Kind != CallFunction || len(call.Signature.Results) != 1 {
		t.Fatalf("dependency function call facts = %#v", call)
	}
	if selection := info.Selections[newCall.Callee.NodeID]; selection.Kind != SelectionPackageMember || selection.ModulePath != "example/lib" {
		t.Fatalf("dependency package selection = %#v", selection)
	}
	sumCall := use.Body.Stmts[1].Right[0]
	if call := info.Calls[sumCall.NodeID]; call.Kind != CallMethod || len(call.Signature.Results) != 2 {
		t.Fatalf("dependency method call facts = %#v operand=%#v type=%s selection=%#v", call, info.Exprs[sumCall.Callee.Operand.NodeID], types.FormatWithTable(info.TypeTable, info.Exprs[sumCall.Callee.Operand.NodeID].Type), info.Selections[sumCall.Callee.NodeID])
	}
	if selection := info.Selections[sumCall.Callee.NodeID]; selection.Kind != SelectionMethod || selection.FunctionID != "method.Counter.Sum" {
		t.Fatalf("dependency method selection = %#v", selection)
	}

	getValue := use.Body.Stmts[2].Right[0]
	addValue := use.Body.Stmts[3].Right[0]
	getExpression := use.Body.Stmts[4].Right[0]
	addExpression := use.Body.Stmts[5].Right[0]
	if selection := info.Selections[getValue.NodeID]; selection.Kind != SelectionMethod || selection.Indirect {
		t.Fatalf("value method selection = %#v", selection)
	}
	if selection := info.Selections[addValue.NodeID]; selection.Kind != SelectionMethod || !selection.Indirect {
		t.Fatalf("auto-address method selection = %#v", selection)
	}
	if selection := info.Selections[getExpression.NodeID]; selection.Kind != SelectionMethodExpression || len(selection.Signature.Params) != 1 {
		t.Fatalf("value method expression = %#v", selection)
	}
	if selection := info.Selections[addExpression.NodeID]; selection.Kind != SelectionMethodExpression || len(selection.Signature.Params) != 2 {
		t.Fatalf("pointer method expression = %#v", selection)
	}
	readCall := use.Body.Stmts[6].Right[0]
	if call := info.Calls[readCall.NodeID]; call.Kind != CallInterfaceMethod || len(call.Signature.Results) != 2 {
		t.Fatalf("interface method call facts = %#v", call)
	}
	returnExpr := use.Body.Stmts[7].Body.Stmts[0].Results[0]
	remoteField := returnExpr.Right
	if remoteField == nil {
		t.Fatal("missing return expression tail")
	}
	for remoteField.Kind == ast.ExprBinary && remoteField.Right != nil {
		remoteField = remoteField.Right
	}
	if selection := info.Selections[remoteField.NodeID]; selection.Kind != SelectionField || selection.Type != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("dependency field selection = %#v", selection)
	}
	importedCall := functions["Imported"].Body.Stmts[0].Right[0]
	if call := info.Calls[importedCall.NodeID]; call.Kind != CallInterfaceMethod || len(call.Signature.Results) != 2 {
		t.Fatalf("dependency interface method call facts = %#v", call)
	}
	if err := info.TypeTable.ValidateCompiler(); err != nil {
		t.Fatalf("validate semantic type table: %v", err)
	}
}

func TestAnalyzeSelectsMethodsAfterConversionAndTypeSwitchNarrowing(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

type Score int64
func (value Score) Value() int64 { return int64(value) }

func Converted() int64 { return Score(1).Value() }
func Narrowed(value any) int64 {
	switch selected := value.(type) {
	case Score:
		return selected.Value()
	default:
		return 0
	}
}
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
	converted := functions["Converted"].Body.Stmts[0].Results[0]
	if call := info.Calls[converted.NodeID]; call.Kind != CallMethod || len(call.Signature.Results) != 1 {
		t.Fatalf("converted receiver call facts = %#v", call)
	}
	narrowed := functions["Narrowed"].Body.Stmts[0].Cases[0].Body.Stmts[0].Results[0]
	if call := info.Calls[narrowed.NodeID]; call.Kind != CallMethod || len(call.Signature.Results) != 1 {
		t.Fatalf("type-switch receiver call facts = %#v", call)
	}
}

func TestAnalyzeBuildsTypeSwitchImplementationFacts(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

type Reader interface { Read() int64 }
type Counter struct { Value int64 }
func (value Counter) Read() int64 { return value.Value }

func Use(value Reader) int64 {
	switch selected := value.(type) {
	case Counter:
		return selected.Read()
	default:
		return 0
	}
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}
	statement := parsed.Program.Files[0].Decls[3].Func.Body.Stmts[0]
	fact := info.Switches[statement.NodeID]
	if fact.Kind != SwitchType || !fact.Interface || fact.TypeSet || len(fact.Cases) != 2 {
		t.Fatalf("type switch facts = %#v", fact)
	}
	if len(fact.Cases[0].Implements) != 1 || !fact.Cases[0].Implements[0] {
		source := fact.Cases[0].Types[0]
		sourceNode, _ := info.TypeTable.Node(source)
		targetNode, _ := info.TypeTable.Node(fact.Subject)
		t.Fatalf("case implementation = false; source=%s %#v target=%s %#v relation=%#v",
			types.FormatWithTable(info.TypeTable, source), sourceNode,
			types.FormatWithTable(info.TypeTable, fact.Subject), targetNode,
			info.Relations.Implements(source, fact.Subject))
	}
	if got := types.FormatWithTable(info.TypeTable, fact.Cases[0].Binding); got != "example/main.Counter" {
		t.Fatalf("single-case binding type = %q", got)
	}
}

func TestAnalyzeBuildsCompositeControlAndValueCategoryFacts(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

type Cell struct { Value int64 }

func Use(cells []Cell, mapping map[string]Cell, pointer *Cell) int64 {
	type Local struct { Value int64 }
	local := Local{Value: 1}
	cells[0].Value = local.Value
	mapping["cell"] = Cell{Value: 2}
	pointer.Value = 3
	switch pointer.Value {
	case 3:
		return cells[0].Value
	default:
		return 0
	}
}
`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := Check(parsed.Program).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}
	body := parsed.Program.Files[0].Decls[1].Func.Body.Stmts
	localDecl := &body[0].Decls[0]
	localComposite := body[1].Right[0]
	composite := info.Composites[localComposite.NodeID]
	wantLocal := fmt.Sprintf("example/main.local_type_%d_Local", localDecl.NodeID)
	if got := types.FormatWithTable(info.TypeTable, composite.Type); got != wantLocal {
		t.Fatalf("local composite type = %q, want %q", got, wantLocal)
	}
	if !composite.TypeExact || composite.Shape != types.Struct || len(composite.Fields) != 1 ||
		composite.Fields[0].Name != "Value" || composite.Fields[0].Type != types.Builtin(types.PrimitiveInt64) {
		t.Fatalf("local composite facts = %#v", composite)
	}

	sliceField := body[2].Left[0]
	if got := info.Exprs[sliceField.NodeID].Category; got != ValueAddressable {
		t.Fatalf("slice field category = %v", got)
	}
	mapIndex := body[3].Left[0]
	if got := info.Exprs[mapIndex.NodeID].Category; got != ValueMapIndex {
		t.Fatalf("map index category = %v", got)
	}
	pointerField := body[4].Left[0]
	if got := info.Exprs[pointerField.NodeID].Category; got != ValueAddressable {
		t.Fatalf("pointer field category = %v", got)
	}

	switchStmt := body[5]
	control := info.Switches[switchStmt.NodeID]
	if control.Kind != SwitchExpression || control.Tag != types.Builtin(types.PrimitiveInt64) || !control.Comparable {
		t.Fatalf("expression switch facts = %#v", control)
	}
}

func TestAnalyzeScopesControlInitializersAndDotImportFunctions(t *testing.T) {
	parsed := parser.ParseSource("example/main", "main.mgo", `package main

import . "example/lib"

type Reader interface { Read() int64 }
type Writer interface { Write() int64 }

func Use(value any) int64 {
	if selected, ok := value.(Reader); ok {
		return selected.Read()
	}
	if selected, ok := value.(Writer); ok {
		return selected.Write()
	}
	return Added(1)
}

`)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse source: %#v", parsed.Diagnostics)
	}
	info := WithOptions(parsed.Program, AnalyzeOptions{Dependencies: testDependencies([]DependencyExport{{
		ModulePath: "example/lib", Name: "Added", ID: "fn.Added", Kind: ObjectFunc, Type: "function(Int64) Int64",
	}})}).Info
	if len(info.Diagnostics) != 0 {
		t.Fatalf("analyze source: %#v", info.Diagnostics)
	}
	var use ast.FuncDecl
	for _, decl := range parsed.Program.Files[0].Decls {
		if decl.Kind == ast.DeclFunc && decl.Func.Name == "Use" {
			use = *decl.Func
		}
	}
	firstIf, secondIf := use.Body.Stmts[0], use.Body.Stmts[1]
	firstID := info.Defs[firstIf.Init.Left[0].NodeID][0]
	secondID := info.Defs[secondIf.Init.Left[0].NodeID][0]
	if firstID == secondID {
		t.Fatalf("separate if initializers reused object %q", firstID)
	}
	firstCall := firstIf.Body.Stmts[0].Results[0]
	secondCall := secondIf.Body.Stmts[0].Results[0]
	if call := info.Calls[firstCall.NodeID]; call.Kind != CallInterfaceMethod || len(call.Signature.Results) != 1 {
		t.Fatalf("first interface call facts = %#v", call)
	}
	if call := info.Calls[secondCall.NodeID]; call.Kind != CallInterfaceMethod || len(call.Signature.Results) != 1 {
		t.Fatalf("second interface call facts = %#v", call)
	}
	dotCall := use.Body.Stmts[2].Results[0]
	if call := info.Calls[dotCall.NodeID]; call.Kind != CallFunction || len(call.Signature.Results) != 1 {
		t.Fatalf("dot-import function call facts = %#v", call)
	}
	dotObject, ok := info.Object(info.Exprs[dotCall.Callee.NodeID].Object)
	if !ok || dotObject.ModulePath != "example/lib" || dotObject.ExportName != "Added" {
		t.Fatalf("dot-import function object = %#v", dotObject)
	}
}
