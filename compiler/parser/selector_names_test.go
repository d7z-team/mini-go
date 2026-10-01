package parser

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
)

func TestSelectorNamesPreserveRoleAndSourceThroughClone(t *testing.T) {
	text := "package sample\nfunc Read() { object.Member }\n"
	result := ParseSource("sample", "main.mgo", text)
	requireNoDiagnostics(t, result)
	if diagnostics, _ := ast.FinalizeStructure(&result.Program, ast.Limits{}); len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	cloned := ast.CloneProgram(result.Program)
	for i := range cloned.Files[0].Decls {
		ast.RewriteDeclSourcePaths(&cloned.Files[0].Decls[i], func(string) string { return "copy.mgo" })
	}
	for _, item := range []struct {
		program ast.Program
		path    string
	}{{result.Program, "main.mgo"}, {cloned, "copy.mgo"}} {
		seen := map[string]bool{}
		for _, name := range ast.Names(item.program) {
			want, ok := map[string]ast.NameRole{"object": ast.NameReference, "Member": ast.NameSelector}[name.Name.Text]
			if !ok {
				continue
			}
			span := name.Name.Span
			if name.Role != want || name.Node == 0 || name.Name.ID == 0 || span.Start.File != item.path || span.End.File != item.path || text[span.Start.Offset:span.End.Offset] != name.Name.Text {
				t.Fatalf("invalid name occurrence: %+v", name)
			}
			if seen[name.Name.Text] {
				t.Fatalf("duplicate occurrence: %s", name.Name.Text)
			}
			seen[name.Name.Text] = true
		}
		if !seen["object"] || !seen["Member"] {
			t.Fatalf("missing names: %v", seen)
		}
	}
}
