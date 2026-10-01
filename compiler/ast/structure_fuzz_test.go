package ast

import "testing"

func FuzzValidateStructure(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4})
	f.Add([]byte{0xff, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		expr := &Expression{Kind: ExprIdent, Name: "value"}
		for _, value := range data {
			switch value % 3 {
			case 0:
				expr = &Expression{Kind: ExprUnary, Operator: "+", Operand: expr}
			case 1:
				expr = &Expression{Kind: ExprCall, Callee: expr, Args: []Expression{{Kind: ExprLiteral, Literal: "0"}}}
			case 2:
				expr = &Expression{Kind: ExprIndex, Operand: expr, Index: &Expression{Kind: ExprLiteral, Literal: "0"}}
			}
		}
		program := Program{Files: []File{{Decls: []Decl{{
			Kind: DeclFunc, Func: &FuncDecl{Body: BlockStmt{Stmts: []Statement{{Kind: StmtExpr, Expr: expr}}}},
		}}}}}
		first := ValidateStructure(&program, Limits{MaxDepth: 64, MaxNodes: 256, MaxDiagnostics: 8})
		second := ValidateStructure(&program, Limits{MaxDepth: 64, MaxNodes: 256, MaxDiagnostics: 8})
		if len(first) != len(second) || len(first) > 9 {
			t.Fatalf("unstable or unbounded diagnostics: %d, %d", len(first), len(second))
		}
		for i := range first {
			if first[i].Code != second[i].Code {
				t.Fatalf("diagnostic order changed: %#v != %#v", first, second)
			}
		}
	})
}
