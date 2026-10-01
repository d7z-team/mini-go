package ast

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
)

func TestDeclarationPayloadsRequireTheirVariantAndRejectCycles(t *testing.T) {
	for _, kind := range []DeclKind{DeclImport, DeclConst, DeclVar, DeclType, DeclFunc} {
		t.Run(string(kind), func(t *testing.T) {
			program := Program{ModulePath: "sample", Package: "sample", Files: []File{{Path: "main.mgo", Decls: []Decl{{Kind: kind}}}}}
			requireDiagnostic(t, ValidateProgram(program), "ast.decl.payload.missing")
			requireDiagnostic(t, ValidateProgramContentsWithLimits(program, Limits{}), "ast.decl.payload.missing")
		})
	}
	function := &FuncDecl{Name: "cycle"}
	function.Body.Stmts = []Statement{{Kind: StmtDecl, Decls: []Decl{{Kind: DeclFunc, Func: function}}}}
	program := Program{Files: []File{{Decls: []Decl{{Kind: DeclFunc, Func: function}}}}}
	for _, assign := range []bool{false, true} {
		var diagnostics []source.Diagnostic
		if assign {
			diagnostics, _ = FinalizeStructure(&program, Limits{})
		} else {
			diagnostics = ValidateStructure(&program, Limits{})
		}
		requireDiagnostic(t, diagnostics, "ast.pointer.cycle")
	}
}

func TestValidateStructureRejectsCyclesAndResourceOverflow(t *testing.T) {
	cycle := &Expression{Kind: ExprUnary}
	cycle.Operand = cycle
	program := Program{Files: []File{{Decls: []Decl{{
		Kind: DeclFunc,
		Func: &FuncDecl{Body: BlockStmt{Stmts: []Statement{{Kind: StmtExpr, Expr: cycle}}}},
	}}}}}
	requireDiagnostic(t, ValidateStructure(&program, Limits{}), "ast.pointer.cycle")

	requireDiagnostic(t, ValidateStructure(&Program{Files: []File{{Path: "main.mgo"}}}, Limits{MaxNodes: 1}), "ast.limit.nodes")

	leaf := &Expression{Kind: ExprIdent, Name: "x"}
	for range 8 {
		leaf = &Expression{Kind: ExprUnary, Operand: leaf}
	}
	program = Program{Files: []File{{Decls: []Decl{{
		Kind: DeclFunc,
		Func: &FuncDecl{Body: BlockStmt{Stmts: []Statement{{Kind: StmtExpr, Expr: leaf}}}},
	}}}}}
	requireDiagnostic(t, ValidateStructure(&program, Limits{MaxDepth: 6}), "ast.limit.depth")
}

func TestValidateStructureChecksNodeIdentityAndSpans(t *testing.T) {
	text := "package main\n"
	file := source.NewFile("main", "main.mgo", text)
	fileSpan, _ := file.Span(0, len(text))
	outside, _ := file.Span(0, len(text))
	outside.End.Offset++
	program := Program{
		NodeID: 1,
		Files: []File{{NodeID: 1, Path: file.Path, Span: fileSpan, Decls: []Decl{{
			NodeID: 2, Kind: DeclInvalid, Span: outside,
		}}}},
	}
	diagnostics := ValidateStructure(&program, Limits{})
	requireDiagnostic(t, diagnostics, "ast.node_id.duplicate")
	requireDiagnostic(t, diagnostics, "ast.span.outside_file")
}

func TestValidateStructureChecksFunctionExpressionPayload(t *testing.T) {
	expression := &Expression{Kind: ExprFunc}
	program := Program{Files: []File{{Decls: []Decl{{Kind: DeclVar, Var: &ValueDecl{Values: []Expression{*expression}}}}}}}
	requireDiagnostic(t, ValidateStructure(&program, Limits{}), "ast.expr.func.missing")
	function := &FuncDecl{Body: BlockStmt{Stmts: []Statement{{Kind: StmtExpr, Expr: expression}}}}
	expression.Func = function
	program.Files[0].Decls[0].Var.Values[0] = *expression
	requireDiagnostic(t, ValidateStructure(&program, Limits{}), "ast.pointer.cycle")
}

func TestSharedSubtreesRespectExpandedNodeAndDepthBudgets(t *testing.T) {
	leaf := &Expression{Kind: ExprIdent, Name: "x"}
	root := &Expression{Kind: ExprBinary, Operator: "+", Left: leaf, Right: leaf}
	program := Program{Files: []File{{Decls: []Decl{{Kind: DeclVar, Var: &ValueDecl{Values: []Expression{*root}}}}}}}
	for _, finalize := range []bool{false, true} {
		diagnostics, stats := walkStructure(&program, Limits{MaxNodes: 6}, finalize)
		if len(diagnostics) != 0 || stats.Nodes != 6 {
			t.Fatalf("shared leaf: finalize=%v nodes=%d diagnostics=%v", finalize, stats.Nodes, diagnostics)
		}
		diagnostics, _ = walkStructure(&program, Limits{MaxNodes: 5}, finalize)
		requireDiagnostic(t, diagnostics, "ast.limit.nodes")
	}
	for range 32 {
		root = &Expression{Kind: ExprBinary, Operator: "+", Left: root, Right: root}
	}
	program.Files[0].Decls[0].Var.Values = []Expression{*root}
	diagnostics, stats := FinalizeStructure(&program, Limits{MaxNodes: 128})
	requireDiagnostic(t, diagnostics, "ast.limit.nodes")
	if stats.Nodes != 129 {
		t.Fatalf("bounded shared traversal counted %d nodes", stats.Nodes)
	}

	shared := &Expression{Kind: ExprUnary, Operand: &Expression{Kind: ExprUnary, Operand: leaf}}
	deep := shared
	for range 8 {
		deep = &Expression{Kind: ExprUnary, Operand: deep}
	}
	program.Files[0].Decls[0].Var.Values = []Expression{
		{Kind: ExprUnary, Operand: shared},
		{Kind: ExprUnary, Operand: deep},
	}
	requireDiagnostic(t, ValidateStructure(&program, Limits{MaxDepth: 12}), "ast.limit.depth")
}

func TestSharedStructureAccountingMatchesIndependentCopies(t *testing.T) {
	for depth := 1; depth <= 8; depth++ {
		node := &Expression{Kind: ExprIdent, Name: "x"}
		for range depth {
			node = &Expression{Kind: ExprBinary, Operator: "+", Left: node, Right: &Expression{Kind: ExprUnary, Operand: node}}
		}
		shared := Program{Files: []File{{Decls: []Decl{{Kind: DeclVar, Var: &ValueDecl{Values: []Expression{*node}}}}}}}
		copied := CloneProgram(shared)
		for _, limits := range []Limits{{}, {MaxNodes: 128}, {MaxDepth: 12}} {
			sharedDiagnostics, sharedStats := ValidateStructureWithStats(&shared, limits)
			copyDiagnostics, copyStats := ValidateStructureWithStats(&copied, limits)
			if sharedStats.Nodes != copyStats.Nodes && len(sharedDiagnostics) == 0 && len(copyDiagnostics) == 0 {
				t.Fatalf("depth %d: shared nodes %d, copied nodes %d", depth, sharedStats.Nodes, copyStats.Nodes)
			}
			if len(sharedDiagnostics) != len(copyDiagnostics) {
				t.Fatalf("depth %d, limits %+v: shared %v, copied %v", depth, limits, sharedDiagnostics, copyDiagnostics)
			}
			for index := range sharedDiagnostics {
				if sharedDiagnostics[index].Code != copyDiagnostics[index].Code {
					t.Fatalf("depth %d, limits %+v: shared %v, copied %v", depth, limits, sharedDiagnostics, copyDiagnostics)
				}
			}
		}
	}
}

func TestExpressionTypeSharesAccountingAndRejectsCycles(t *testing.T) {
	typ := &TypeExpr{Kind: TypeName, Name: "Int"}
	program := Program{Files: []File{{Decls: []Decl{{Kind: DeclVar, Var: &ValueDecl{Values: []Expression{
		{Kind: ExprLiteral, Literal: "1", Type: typ},
		{Kind: ExprLiteral, Literal: "2", Type: typ},
	}}}}}}}
	independent := CloneProgram(program)
	diagnostics, shared := FinalizeStructure(&program, Limits{})
	copyDiagnostics, copied := FinalizeStructure(&independent, Limits{})
	if len(diagnostics) != 0 || len(copyDiagnostics) != 0 || shared.Nodes != copied.Nodes {
		t.Fatalf("shared type accounting: %v/%v, %d/%d", diagnostics, copyDiagnostics, shared.Nodes, copied.Nodes)
	}
	requireDiagnostic(t, ValidateStructure(&program, Limits{MaxNodes: shared.Nodes - 1}), "ast.limit.nodes")
	typ.Kind = TypeArray
	typ.Len = &program.Files[0].Decls[0].Var.Values[0]
	requireDiagnostic(t, ValidateStructure(&program, Limits{}), "ast.pointer.cycle")
}
