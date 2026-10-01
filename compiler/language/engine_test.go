package language

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestCanceledChangeKeepsCommittedSnapshot(t *testing.T) {
	engine, uri := testEngine(t, "package main\nfunc main() {}\n")
	before := engine.Snapshot()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.ChangeContext(ctx, uri, 1, []ContentChange{{Text: "package main\nfunc changed() {}\n"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChangeContext canceled context = %v", err)
	}
	after := engine.Snapshot()
	if after != before || after.generation != before.generation || after.documents[uri].Text != before.documents[uri].Text {
		t.Fatal("canceled edit replaced the committed snapshot")
	}
}

func TestGenericMethodDefinitionAndHover(t *testing.T) {
	engine, uri := testEngine(t, "package main\ntype Box struct{}\nfunc (b Box) Echo[T any](value T) T { return value }\nfunc Main() int { return Box{}.Echo(1) }\n")
	use := Position{Line: 3, Character: 31}
	definitions := engine.Definition(uri, use)
	if len(definitions) != 1 || definitions[0].Range.Start.Line != 2 {
		t.Fatalf("method definitions: %+v", definitions)
	}
	hover := engine.Hover(uri, use)
	if hover == nil || !strings.Contains(hover.Contents.Value, "T Any") {
		t.Fatalf("method hover: %+v", hover)
	}
}

func TestPromotedFieldDefinitionUsesDeclaringType(t *testing.T) {
	text := "package main\ntype Inner[T any] struct{ X T }\ntype Outer struct{ Inner[int] }\nfunc Main() { _ = Outer{X:1}; value := Outer{}; _ = value.X }\n"
	engine, uri := testEngine(t, text)
	for _, offset := range []int{strings.Index(text, "X:1"), strings.LastIndex(text, ".X") + 1} {
		position, err := engine.Snapshot().documents[uri].Index.Position(offset)
		if err != nil {
			t.Fatal(err)
		}
		definitions := engine.Definition(uri, position)
		if len(definitions) != 1 || definitions[0].Range.Start.Line != 1 || definitions[0].URI != uri {
			t.Fatalf("promoted field definition at %d: %+v", offset, definitions)
		}
		if hover := engine.Hover(uri, position); hover == nil || !strings.Contains(hover.Contents.Value, "X Int") {
			t.Fatalf("promoted field hover: %+v", hover)
		}
	}
}

func TestImportedPromotedFieldDefinitionAndHover(t *testing.T) {
	text := "package main\nimport \"example/lib\"\nfunc Main() { _ = lib.Outer{X:1}; value := lib.Outer{}; _ = value.X }\n"
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: text}}},
		{ModulePath: "example/lib", Files: []source.File{{Path: "lib.mgo", Text: "package lib\ntype Inner[T any] struct{ X T }\ntype Alias = Inner[int]\ntype Outer struct{ Alias }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: set, URI: func(module, path string) DocumentURI { return DocumentURI("file:///" + module + "/" + path) }})
	if err != nil {
		t.Fatal(err)
	}
	uri := DocumentURI("file:///example/main.mgo")
	for _, offset := range []int{strings.Index(text, "X:1"), strings.LastIndex(text, ".X") + 1} {
		position, err := engine.Snapshot().documents[uri].Index.Position(offset)
		if err != nil {
			t.Fatal(err)
		}
		definitions := engine.Definition(uri, position)
		if len(definitions) != 1 || definitions[0].Range.Start.Line != 1 || definitions[0].URI != "file:///example/lib/lib.mgo" {
			t.Fatalf("imported field definition at %d: %+v", offset, definitions)
		}
		if hover := engine.Hover(uri, position); hover == nil || !strings.Contains(hover.Contents.Value, "X Int") {
			t.Fatalf("imported field hover: %+v", hover)
		}
	}
}

func TestImportedGenericMethodDefinitionAndHover(t *testing.T) {
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/lib\"\nfunc Main() int { return lib.Box{}.Echo(1) }\n"}}},
		{ModulePath: "example/lib", Files: []source.File{{Path: "lib.mgo", Text: "package lib\ntype Box struct{}\nfunc (Box) Echo[Int ~int](value Int) Int { return value }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: set, URI: func(module, path string) DocumentURI { return DocumentURI("file:///" + module + "/" + path) }})
	if err != nil {
		t.Fatal(err)
	}
	uri := DocumentURI("file:///example/main.mgo")
	use := Position{Line: 2, Character: 35}
	definitions := engine.Definition(uri, use)
	if len(definitions) != 1 || definitions[0].Range.Start.Line != 2 || definitions[0].URI != "file:///example/lib/lib.mgo" {
		t.Fatalf("imported method definition: %+v", definitions)
	}
	hover := engine.Hover(uri, use)
	if hover == nil || !strings.Contains(hover.Contents.Value, "Int") || strings.Contains(hover.Contents.Value, "_method_parameter_") {
		t.Fatalf("imported method hover: %+v", hover)
	}
}

func TestSelectionRangesFollowExpressionBlockAndFunction(t *testing.T) {
	text := "package main\nfunc Main() int {\n\tif true { return (1 + 2) * 3 }; return 0\n}\n"
	engine, uri := testEngine(t, text)
	document := engine.Snapshot().documents[uri]
	position, err := document.Index.Position(strings.Index(text, "2)"))
	if err != nil {
		t.Fatal(err)
	}
	ranges := engine.SelectionRanges(uri, []Position{position})
	if len(ranges) != 1 {
		t.Fatal("missing selection")
	}
	seen := map[string]bool{}
	for current := &ranges[0]; current != nil; current = current.Parent {
		start, err := document.Index.Offset(current.Range.Start)
		if err != nil {
			t.Fatal(err)
		}
		end, err := document.Index.Offset(current.Range.End)
		if err != nil {
			t.Fatal(err)
		}
		seen[text[start:end]] = true
		if current.Parent != nil && (positionLess(current.Range.Start, current.Parent.Range.Start) || positionLess(current.Parent.Range.End, current.Range.End)) {
			t.Fatal("selection escaped its parent")
		}
	}
	for _, expected := range []string{"2", "(1 + 2)", "(1 + 2) * 3", "{ return (1 + 2) * 3 }", text} {
		if !seen[expected] {
			t.Fatalf("missing syntax selection %q: %#v", expected, seen)
		}
	}
}

func testEngine(t *testing.T, text string) (*Engine, DocumentURI) {
	t.Helper()
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: text}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: set, URI: func(_, _ string) DocumentURI { return "file:///main.mgo" }})
	if err != nil {
		t.Fatal(err)
	}
	return engine, "file:///main.mgo"
}

func TestEngineUsesConfiguredBuildTags(t *testing.T) {
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example",
		Files: []source.File{
			{Path: "main.mgo", Text: "package main\nfunc main() { _ = mode() }\n"},
			{Path: "debug.mgo", Text: "//go:build debug\n\npackage main\nfunc mode() int { return 1 }\n"},
			{Path: "release.mgo", Text: "//go:build !debug\n\npackage main\nfunc mode( {\n"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Target: target.Target{Tags: []string{"debug"}}, Sources: set})
	if err != nil {
		t.Fatal(err)
	}
	for uri, diagnostics := range engine.Snapshot().diagnostics {
		if len(diagnostics) != 0 {
			t.Fatalf("diagnostics for %s = %#v", uri, diagnostics)
		}
	}
}

func TestEngineIndexesDependenciesAsReadOnlyDocuments(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"dependency\"\nfunc main() { _ = dependency.Value() }\n"}}},
		{ModulePath: "dependency", Files: []source.File{{Path: "value.mgo", Text: "package dependency\nfunc Value() int { return 1 }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"dependency\"\nfunc main() { _ = dependency.Value() }\n"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: sources, Documents: documents})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if len(snapshot.documents) != 2 {
		t.Fatalf("documents = %#v", snapshot.documents)
	}
	editable := 0
	for _, value := range snapshot.editable {
		if value {
			editable++
		}
	}
	if editable != 1 {
		t.Fatalf("editable documents = %#v", snapshot.editable)
	}
	if _, ok := snapshot.packages["dependency"]; !ok {
		t.Fatalf("dependency missing from analysis packages: %#v", snapshot.packages)
	}
	var rootURI DocumentURI
	for uri, editable := range snapshot.editable {
		if editable {
			rootURI = uri
		}
	}
	definitions := engine.Definition(rootURI, Position{Line: 2, Character: 31})
	if len(definitions) != 1 || !strings.HasPrefix(string(definitions[0].URI), "mini-go://dependency/") {
		t.Fatalf("dependency definitions = %#v", definitions)
	}
}

func TestEngineNavigationRenameAndEnhancedQueries(t *testing.T) {
	engine, uri := testEngine(t, "package main\nfunc value() int { return 1 }\nfunc main() { _ = value() }\n")
	definitionPosition := Position{Line: 1, Character: 5}
	usePosition := Position{Line: 2, Character: 18}
	definitions := engine.Definition(uri, usePosition)
	if len(definitions) != 1 || definitions[0].Range.Start != definitionPosition {
		t.Fatalf("definitions = %+v", definitions)
	}
	if references := engine.References(uri, usePosition, true); len(references) != 2 {
		t.Fatalf("references = %+v", references)
	}
	edit, err := engine.Rename(uri, usePosition, "answer")
	if err != nil || len(edit.DocumentChanges) != 1 || len(edit.DocumentChanges[0].Edits) != 2 {
		t.Fatalf("rename = %+v, %v", edit, err)
	}
	if hover := engine.Hover(uri, usePosition); hover == nil {
		t.Fatal("missing hover")
	}
	if symbols := engine.DocumentSymbols(uri); len(symbols) < 2 {
		t.Fatalf("symbols = %+v", symbols)
	}
	if highlights := engine.Highlights(uri, usePosition); len(highlights) != 2 {
		t.Fatalf("highlights = %+v", highlights)
	}
	if tokens := engine.SemanticTokens(uri, nil); len(tokens.Data) == 0 {
		t.Fatal("missing semantic tokens")
	}
	if ranges := engine.SelectionRanges(uri, []Position{definitionPosition}); len(ranges) != 1 || ranges[0].Parent == nil {
		t.Fatalf("selection ranges = %+v", ranges)
	}
}

func TestEngineUsesCatalogDocumentation(t *testing.T) {
	engine, uri := testEngine(t, "package main\n// Value returns the documented result.\nfunc Value() int { return 1 }\nfunc main() { _ = Value() }\n")
	use := Position{Line: 3, Character: 18}
	hover := engine.Hover(uri, use)
	if hover == nil || !strings.Contains(hover.Contents.Value, "Value returns the documented result.") {
		t.Fatalf("hover = %#v", hover)
	}
	completion := engine.Completion(uri, use)
	found := false
	for _, item := range completion.Items {
		if item.Label == "Value" && strings.Contains(item.Documentation.Value, "documented result") {
			found = true
		}
	}
	if !found {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestEngineIncrementalDiagnosticsAndFormatting(t *testing.T) {
	engine, uri := testEngine(t, "package main\nfunc main(){println(1)}\n")
	if edits, err := engine.Format(uri); err != nil || len(edits) != 1 {
		t.Fatalf("format = %+v, %v", edits, err)
	}
	document := engine.Snapshot().documents[uri]
	if err := engine.Open(document.Identity, 1, "package main\nfunc main( {\n"); err != nil {
		t.Fatal(err)
	}
	if !engine.Snapshot().partial {
		t.Fatal("syntax-error fallback snapshot was not marked partial")
	}
	report := engine.Diagnostics(uri, "")
	if report.Kind != "full" || len(report.Items) == 0 {
		t.Fatalf("diagnostics = %+v", report)
	}
	if unchanged := engine.Diagnostics(uri, report.ResultID); unchanged.Kind != "unchanged" || len(unchanged.Items) != 0 {
		t.Fatalf("unchanged = %+v", unchanged)
	}
}

func TestEnginePublishesAndClearsImportCycleDiagnostics(t *testing.T) {
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example/a", Files: []source.File{{Path: "a/a.mgo", Text: "package a\nimport \"example/b\"\n"}}},
		{ModulePath: "example/b", Files: []source.File{{Path: "b/b.mgo", Text: "package b\nimport \"example/a\"\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example/a", Sources: set})
	if err != nil {
		t.Fatal(err)
	}
	var dependencyURI DocumentURI
	for uri, document := range engine.Snapshot().documents {
		if document.Identity.ModulePath == "example/b" {
			dependencyURI = uri
		}
	}
	diagnostics := engine.Snapshot().diagnostics[dependencyURI]
	if len(diagnostics) != 1 || diagnostics[0].Code != "compiler.workspace.module.cycle" || diagnostics[0].Range.Start.Line != 1 {
		t.Fatalf("cycle diagnostics = %#v", diagnostics)
	}
	document := engine.Snapshot().documents[dependencyURI]
	if err := engine.Open(document.Identity, 1, "package b\n"); err != nil {
		t.Fatal(err)
	}
	for uri, diagnostics := range engine.Snapshot().diagnostics {
		for _, diagnostic := range diagnostics {
			if diagnostic.Code == "compiler.workspace.module.cycle" {
				t.Fatalf("cycle diagnostic remained for %s: %#v", uri, diagnostic)
			}
		}
	}
}

func TestEngineMutationCommitsStoreAndSnapshotTogether(t *testing.T) {
	engine, _ := testEngine(t, "package main\nfunc main() {}\n")
	before := engine.Snapshot()
	invalid := DocumentIdentity{URI: "file:///binding.go", ModulePath: "example", Path: "binding.go"}
	if err := engine.Open(invalid, 1, "package main\n"); err == nil {
		t.Fatal("open accepted a non-source overlay")
	}
	if engine.Snapshot() != before {
		t.Fatal("failed mutation replaced the snapshot")
	}
	if _, exists := engine.store.Document(invalid.URI); exists {
		t.Fatal("failed mutation changed the document store")
	}
	if _, exists := engine.editable[invalid.URI]; exists {
		t.Fatal("failed mutation changed the editable document set")
	}

	created := DocumentIdentity{URI: "file:///created.mgo", ModulePath: "example", Path: "created.mgo"}
	if err := engine.Open(created, 1, "package main\nfunc created() {}\n"); err != nil {
		t.Fatal(err)
	}
	if !engine.Snapshot().editable[created.URI] {
		t.Fatal("new workspace source is not editable")
	}
	if err := engine.Close(created.URI); err != nil {
		t.Fatal(err)
	}
	if _, exists := engine.Snapshot().documents[created.URI]; exists {
		t.Fatal("closing a new overlay retained the document")
	}
	if _, exists := engine.editable[created.URI]; exists {
		t.Fatal("closing a new overlay retained editable state")
	}
}

func TestEngineOrganizesUnusedImports(t *testing.T) {
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example/dep", Files: []source.File{{Path: "dep/dep.mgo", Text: "package dep\nfunc Value() int { return 1 }\n"}}},
		{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/dep\"\nfunc main() {}\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: set})
	if err != nil {
		t.Fatal(err)
	}
	var uri DocumentURI
	for candidate, document := range engine.Snapshot().documents {
		if document.Identity.ModulePath == "example" {
			uri = candidate
		}
	}
	edits, err := engine.OrganizeImports(uri)
	if err != nil || len(edits) != 1 || strings.Contains(edits[0].NewText, "example/dep") {
		t.Fatalf("organize imports = %+v, %v", edits, err)
	}
}

func TestEngineReusesUnchangedPackageSnapshot(t *testing.T) {
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example/dep", Files: []source.File{{Path: "dep/dep.mgo", Text: "package dep\nfunc Value() int { return 1 }\n"}}},
		{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/dep\"\nfunc main() { _ = dep.Value() }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: set})
	if err != nil {
		t.Fatal(err)
	}
	var root Document
	for _, document := range engine.Snapshot().documents {
		if document.Identity.ModulePath == "example" {
			root = document
		}
	}
	if err := engine.Open(root.Identity, 1, root.Text+"\n"); err != nil {
		t.Fatal(err)
	}
	if engine.Snapshot().reusedPackages != 1 || engine.Snapshot().checkedPackages != 1 {
		t.Fatalf("snapshot reuse: reused=%d checked=%d", engine.Snapshot().reusedPackages, engine.Snapshot().checkedPackages)
	}
}

func TestEngineInvalidatesDependentPackageWhenExportChanges(t *testing.T) {
	set, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
		{ModulePath: "example/dep", Files: []source.File{{Path: "dep/dep.mgo", Text: "package dep\nfunc Value() int { return 1 }\n"}}},
		{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nimport \"example/dep\"\nfunc main() { _ = dep.Value() }\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(Config{Root: "example", Sources: set})
	if err != nil {
		t.Fatal(err)
	}
	var dependency Document
	for _, document := range engine.Snapshot().documents {
		if document.Identity.ModulePath == "example/dep" {
			dependency = document
		}
	}
	if err := engine.Open(dependency.Identity, 1, "package dep\nfunc Value() string { return \"one\" }\n"); err != nil {
		t.Fatal(err)
	}
	if engine.Snapshot().reusedPackages != 0 || engine.Snapshot().checkedPackages != 2 {
		t.Fatalf("snapshot invalidation: reused=%d checked=%d", engine.Snapshot().reusedPackages, engine.Snapshot().checkedPackages)
	}
}

func TestEngineNavigatesMethodFieldTypeAndLabel(t *testing.T) {
	engine, uri := testEngine(t, "package main\ntype item struct { value int }\nfunc (x item) get() int { return x.value }\nfunc main() { var x item; _ = x.get(); goto done; done: _ = x.value }\n")
	cases := []Position{{2, 36}, {3, 33}, {3, 47}, {3, 64}}
	for _, position := range cases {
		if definitions := engine.Definition(uri, position); len(definitions) != 1 {
			t.Errorf("Definition(%+v) = %+v", position, definitions)
		}
	}
}

func TestEngineTypeDefinitionImplementationAndScopedCompletion(t *testing.T) {
	engine, uri := testEngine(t, "package main\ntype reader interface { Read() int }\ntype value struct{}\nfunc (value) Read() int { return 1 }\nfunc use() { var item value; _ = item }\nfunc hidden() { secret := 1; _ = secret }\n")
	if definitions := engine.TypeDefinition(uri, Position{Line: 4, Character: 33}); len(definitions) != 1 || definitions[0].Range.Start != (Position{Line: 2, Character: 5}) {
		t.Fatalf("type definitions = %+v", definitions)
	}
	if implementations := engine.Implementation(uri, Position{Line: 1, Character: 6}); len(implementations) != 1 || implementations[0].Range.Start != (Position{Line: 2, Character: 5}) {
		t.Fatalf("implementations = %+v", implementations)
	}
	completion := engine.Completion(uri, Position{Line: 4, Character: 37})
	labels := map[string]bool{}
	for _, item := range completion.Items {
		labels[item.Label] = true
	}
	if !labels["item"] || labels["secret"] {
		t.Fatalf("completion labels = %+v", labels)
	}
}

func TestEngineSignatureHelpUsesInnermostCallAndSemanticRange(t *testing.T) {
	text := "package main\nfunc inner(left, right int) int { return left + right }\nfunc outer(value int) {}\nfunc main() { outer(inner(1, 2)) }\n"
	engine, uri := testEngine(t, text)
	document := engine.Snapshot().documents[uri]
	cursor, err := document.Index.Position(strings.Index(text, "2))") + 1)
	if err != nil {
		t.Fatal(err)
	}
	help := engine.SignatureHelp(uri, cursor)
	if help == nil || len(help.Signatures) != 1 || !strings.HasPrefix(help.Signatures[0].Label, "inner ") || help.ActiveParameter != 1 {
		t.Fatalf("signature help = %#v", help)
	}
	start := strings.LastIndex(text, "inner(1")
	selected, err := document.Index.Range(start, start+len("inner"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := engine.SemanticTokens(uri, &selected)
	if len(tokens.Data) != 5 || int(tokens.Data[0]) != selected.Start.Line || int(tokens.Data[1]) != selected.Start.Character {
		t.Fatalf("range semantic tokens = %#v", tokens.Data)
	}
}

func TestEngineSelectorCompletionUsesReceiverType(t *testing.T) {
	text := "package main\ntype record struct { Value int }\nfunc (record) Read() int { return 1 }\nfunc use() { var item record; _ = item.Value }\n"
	engine, uri := testEngine(t, text)
	document := engine.Snapshot().documents[uri]
	position, err := document.Index.Position(strings.Index(text, "item.Value") + len("item."))
	if err != nil {
		t.Fatal(err)
	}
	completion := engine.Completion(uri, position)
	labels := map[string]bool{}
	for _, item := range completion.Items {
		labels[item.Label] = true
	}
	if !labels["Value"] || !labels["Read"] || labels["item"] {
		t.Fatalf("selector completion = %#v", labels)
	}
}

func TestEngineSymbolKindsUseSemanticObjects(t *testing.T) {
	engine, uri := testEngine(t, "package main\ntype record struct { Value int }\nconst Limit = 1\nvar Current int\nfunc (record) Read() int { return 1 }\nfunc Run() {}\nfunc main() {}\n")
	want := map[string]int{"record": 23, "Value": 8, "Limit": 14, "Current": 13, "Read": 6, "Run": 12}
	for _, symbol := range engine.DocumentSymbols(uri) {
		if kind, exists := want[symbol.Name]; exists {
			if symbol.Kind != kind {
				t.Errorf("document symbol %s kind = %d, want %d", symbol.Name, symbol.Kind, kind)
			}
			delete(want, symbol.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing document symbols: %#v", want)
	}
	found := false
	for _, symbol := range engine.WorkspaceSymbols("Run") {
		if symbol.Name == "Run" && (symbol.Kind != 12 || symbol.ContainerName != "example") {
			t.Fatalf("workspace symbol = %#v", symbol)
		}
		found = found || symbol.Name == "Run"
	}
	if !found {
		t.Fatal("missing Run workspace symbol")
	}
}

func BenchmarkEngineLocalEdit(b *testing.B) {
	set, _ := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: "package main\nfunc main() { value := 1; _ = value }\n"}}}})
	engine, _ := NewEngine(Config{Root: "example", Sources: set})
	var document Document
	for _, document = range engine.Snapshot().documents {
	}
	b.ResetTimer()
	for iteration := 1; iteration <= b.N; iteration++ {
		text := document.Text
		if iteration%2 == 0 {
			text += "\n"
		}
		_ = engine.Open(document.Identity, iteration, text)
	}
}
