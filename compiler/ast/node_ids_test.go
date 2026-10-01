package ast

import "testing"

func TestFinalizeStructureAssignsDeterministicIdentities(t *testing.T) {
	program := Program{
		ModulePath: "example/main",
		Package:    "main",
		Files: []File{
			{
				Path: "a.mgo",
				Decls: []Decl{{
					Kind: DeclFunc,
					Func: &FuncDecl{
						Name: "first",
						Body: BlockStmt{Stmts: []Statement{{
							Kind: StmtReturn,
							Results: []Expression{{
								Kind:    ExprLiteral,
								Literal: "1",
								Type:    &TypeExpr{Kind: TypeName, Name: "Int"},
							}},
						}}},
					},
				}},
			},
			{
				Path: "b.mgo",
				Decls: []Decl{{
					Kind: DeclVar,
					Var: &ValueDecl{
						Names:  []string{"second"},
						Values: []Expression{{Kind: ExprIdent, Name: "first"}},
					},
				}},
			},
		},
	}

	if diagnostics, _ := FinalizeStructure(&program, Limits{}); len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	first := collectTestNodeIDs(program)
	copyOfProgram := program
	if diagnostics, _ := FinalizeStructure(&copyOfProgram, Limits{}); len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	second := collectTestNodeIDs(copyOfProgram)
	if len(first) != len(second) {
		t.Fatalf("node count changed after reassignment: %d != %d", len(first), len(second))
	}
	seen := make(map[NodeID]struct{}, len(first))
	for i, id := range first {
		if id == 0 {
			t.Fatalf("node %d has zero ID", i)
		}
		if id != second[i] {
			t.Fatalf("node %d ID changed: %d != %d", i, id, second[i])
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate node ID %d", id)
		}
		seen[id] = struct{}{}
	}
	if program.Files[0].NodeID >= program.Files[1].NodeID {
		t.Fatalf("file IDs do not preserve source order: %d >= %d", program.Files[0].NodeID, program.Files[1].NodeID)
	}
}

func collectTestNodeIDs(program Program) []NodeID {
	ids := []NodeID{program.NodeID}
	for _, file := range program.Files {
		ids = append(ids, file.NodeID)
		for _, decl := range file.Decls {
			ids = append(ids, decl.NodeID)
			switch decl.Kind {
			case DeclFunc:
				ids = append(ids, decl.Func.NodeID, decl.Func.Body.NodeID)
				for _, stmt := range decl.Func.Body.Stmts {
					ids = append(ids, stmt.NodeID)
					for _, expr := range stmt.Results {
						ids = append(ids, expr.NodeID)
						if expr.Type != nil {
							ids = append(ids, expr.Type.NodeID)
						}
					}
				}
			case DeclVar:
				for _, expr := range decl.Var.Values {
					ids = append(ids, expr.NodeID)
				}
			}
		}
	}
	return ids
}
