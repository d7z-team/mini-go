package workspace

import (
	"reflect"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
)

func TestScanPackageHeaderKeepsFirstImportLocation(t *testing.T) {
	text := "package app\nimport \"example/z\"\nimport (\n alias \"example/z\"\n . \"example/a\"\n _ \"embed\"\n)\n"
	pkg := SourcePackage{ModulePath: "app", Files: []source.File{
		{Path: "b.mgo", Text: "package app\nimport \"example/z\"\n"},
		{Path: "a.mgo", OriginPath: "original/a.go", Text: text},
	}}
	header, diagnostics, err := ScanPackageHeader(pkg)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("header: %v, %v", err, diagnostics)
	}
	if !reflect.DeepEqual(header.Imports, []string{"example/a", "example/z"}) {
		t.Fatalf("imports: %v", header.Imports)
	}
	span := header.importSpans["example/z"]
	if span.Start.File != "original/a.go" || span.Start.Offset != strings.Index(text, `"example/z"`) {
		t.Fatalf("first import location: %+v", span)
	}
}

func TestAffectedTestPackagesSeparatesProductionAndTestEdits(t *testing.T) {
	sources, err := NewMemorySourceSet([]SourcePackage{
		{ModulePath: "example/base", Files: []source.File{{Path: "base.mgo", Text: "package base\n"}}, TestFiles: []source.File{{Path: "base_test.mgo", Text: "package base\n"}}},
		{ModulePath: "example/middle", Files: []source.File{{Path: "middle.mgo", Text: "package middle\nimport \"example/base\"\n"}}, TestFiles: []source.File{{Path: "middle_test.mgo", Text: "package middle\n"}}},
		{ModulePath: "example/top", Files: []source.File{{Path: "top.mgo", Text: "package top\nimport \"example/middle\"\n"}}, TestFiles: []source.File{{Path: "top_test.mgo", Text: "package top\n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	production, err := AffectedTestPackages(sources, []ChangedFile{{ModulePath: "example/base", Path: "base.mgo"}}, target.Target{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example/base", "example/middle", "example/top"}; !reflect.DeepEqual(production, want) {
		t.Fatalf("production impact = %#v, want %#v", production, want)
	}
	testOnly, err := AffectedTestPackages(sources, []ChangedFile{{ModulePath: "example/base", Path: "base_test.mgo"}}, target.Target{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example/base"}; !reflect.DeepEqual(testOnly, want) {
		t.Fatalf("test impact = %#v, want %#v", testOnly, want)
	}
}

func TestScanPackageHeaderMatchesParsedImports(t *testing.T) {
	pkg := SourcePackage{ModulePath: "example/main", Files: []source.File{{Path: "main.mgo", Text: `package main
import (
	alias "example/one"
	. "example/two"
	_ "example/three"
)
func main() {}
`}}}
	header, diagnostics, err := ScanPackageHeader(pkg)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("scan header: header=%#v diagnostics=%#v err=%v", header, diagnostics, err)
	}
	parsed, diagnostics, err := ParsePackage(pkg)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("parse package: parsed=%#v diagnostics=%#v err=%v", parsed, diagnostics, err)
	}
	if !reflect.DeepEqual(header.Imports, parsed.Imports) || header.Package != parsed.Program.Package {
		t.Fatalf("header=%#v parsed imports=%#v package=%q", header, parsed.Imports, parsed.Program.Package)
	}
}
