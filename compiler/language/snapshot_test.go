package language

import (
	"context"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestForkedOccurrenceQueriesRetainTheirSourceRevision(t *testing.T) {
	text := "package example\nfunc Answer() int { return 42 }\nfunc Main() int { return Answer() }\n"
	engine, uri := testEngine(t, text)
	document := engine.Snapshot().documents[uri]
	position, err := document.Index.Position(strings.LastIndex(text, "Answer"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := engine.Fork()
	if err := candidate.ApplyDocuments([]DocumentUpdate{{Operation: "open", Identity: document.Identity, Version: 1, Text: strings.ReplaceAll(text, "Answer", "Result")}}); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Analyze(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		engine *Engine
		name   string
	}{{engine, "Answer"}, {candidate, "Result"}} {
		hover := query.engine.Hover(uri, position)
		if hover == nil || !strings.Contains(hover.Contents.Value, query.name) {
			t.Fatalf("%s hover lost its revision: %#v", query.name, hover)
		}
		if definitions := query.engine.Definition(uri, position); len(definitions) != 1 || definitions[0].URI != uri {
			t.Fatalf("%s definition = %#v", query.name, definitions)
		}
		if references := query.engine.References(uri, position, true); len(references) != 2 {
			t.Fatalf("%s references = %#v", query.name, references)
		}
	}
}

func TestSnapshotViewsOwnMutableResults(t *testing.T) {
	engine, uri := testEngine(t, "package example\nfunc Answer() int {return 42}\n")
	snapshot := engine.Snapshot()
	snapshot.diagnostics[uri] = []Diagnostic{{Message: "diagnostic", RelatedInformation: []DiagnosticRelatedInformation{{Message: "related"}}}}
	snapshot.workspaceDiagnostics = []source.Diagnostic{{Message: "workspace", Related: []source.RelatedDiagnostic{{Message: "related"}}}}
	documents := snapshot.Documents()
	delete(documents, uri)
	diagnostics := snapshot.Diagnostics(uri)
	diagnostics[0].RelatedInformation[0].Message = "changed"
	workspace := snapshot.WorkspaceDiagnostics()
	workspace[0].Related[0].Message = "changed"
	if _, found := snapshot.Documents()[uri]; !found {
		t.Fatal("document view mutated snapshot")
	}
	if snapshot.Diagnostics(uri)[0].RelatedInformation[0].Message != "related" || snapshot.WorkspaceDiagnostics()[0].Related[0].Message != "related" {
		t.Fatal("diagnostic view mutated snapshot")
	}
}

func TestReadOnlyDependencyCannotBecomeAnEditableOverlay(t *testing.T) {
	app, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "app", Files: []source.File{{Path: "main.mgo", Text: "package app\nimport \"dep\"\nfunc Value() int {return dep.Value()}"}}}})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "dep", Files: []source.File{{Path: "main.mgo", Text: "package dep\nfunc Value() int {return 1}"}}}})
	if err != nil {
		t.Fatal(err)
	}
	all, err := workspace.MergeSourceSets(app, dep)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "app", Sources: all, Documents: app})
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	err = engine.ApplyDocuments([]DocumentUpdate{{Operation: "open", Identity: DocumentIdentity{URI: "mini-go://dep/main.mgo", ModulePath: "dep", Path: "main.mgo"}, Version: 1, Text: "package dep"}})
	if err == nil {
		t.Fatal("read-only dependency edit accepted")
	}
	if engine.Snapshot() != before {
		t.Fatal("rejected edit changed analysis")
	}
}
