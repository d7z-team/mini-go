package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"go/build"
	"go/importer"
	gotypes "go/types"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckedInAPIReferences(t *testing.T) {
	baseline, err := readSnapshot(filepath.Join("testdata", filepath.Base(baselinePath)))
	if err != nil {
		t.Fatal(err)
	}
	jsonReference, err := readSnapshot(filepath.Join("testdata", filepath.Base(jsonBaselinePath)))
	if err != nil {
		t.Fatal(err)
	}
	if baseline.GoVersion != goVersion || jsonReference.GoVersion != jsonGoVersion {
		t.Fatal("reference domain versions")
	}
	baseline.Packages = append(baseline.Packages, jsonReference.Packages...)
	paths, err := packagePaths()
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool)
	for _, pkg := range baseline.Packages {
		covered[pkg.Path] = true
	}
	for _, path := range paths {
		pkg, err := build.Default.Import(path, "", build.FindOnly)
		if (path == "uuid" || err == nil && pkg.Goroot) && !covered[path] {
			t.Errorf("standard package %s is missing from API references", path)
		}
		delete(covered, path)
	}
	for path := range covered {
		t.Errorf("API reference has no source package: %s", path)
	}
	actual, err := buildMiniGoSnapshot(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if differences := compareSnapshots(baseline, actual); len(differences) != 0 {
		for _, diff := range differences {
			t.Errorf("%s %s %s: %s want %s got %s", diff.Package, diff.Name, diff.Kind, diff.Code, diff.Want, diff.Got)
		}
	}
}

func TestGenericMethodAPIConstraints(t *testing.T) {
	want := apiDeclaration{Kind: "type", Name: "Rand", Type: "rand.Rand", Methods: []apiField{{Name: "N", Type: "function(T) T", TypeParams: []apiField{{Name: "T", Type: "interface{~Int}"}}}}}
	got := want
	got.Methods = []apiField{{Name: "N", Type: "function(T) T", TypeParams: []apiField{{Name: "T", Type: "Any"}}}}
	if declarationMatches(want, got, false) {
		t.Fatal("method constraint mismatch accepted")
	}
	baseline := snapshot{Packages: []apiPackage{{Path: "math/rand/v2", Declarations: []apiDeclaration{want}}}}
	actual := snapshot{Packages: []apiPackage{{Path: "math/rand/v2", Declarations: []apiDeclaration{{Kind: "type", Name: "Rand", Type: "rand.Rand"}}}}}
	if len(compareSnapshots(baseline, actual)) == 0 {
		t.Fatal("required method absent")
	}
}

func TestGoTypeMembersIncludesPromotedTestingMethods(t *testing.T) {
	pkg, err := importer.Default().Import("testing")
	if err != nil {
		t.Fatal(err)
	}
	typeObject, ok := pkg.Scope().Lookup("T").(*gotypes.TypeName)
	if !ok {
		t.Fatal("testing.T type is missing")
	}
	_, methods := goTypeMembers(typeObject.Type(), pkg)
	for _, method := range methods {
		if method.Name == "Error" {
			return
		}
	}
	t.Fatalf("testing.T promoted Error method missing: %+v", methods)
}

func TestGoTypePreservesGenericConstraints(t *testing.T) {
	cmpPackage, err := importer.Default().Import("cmp")
	if err != nil {
		t.Fatal(err)
	}
	or := cmpPackage.Scope().Lookup("Or").(*gotypes.Func).Signature()
	if got := goType(or.TypeParams().At(0).Constraint(), cmpPackage); got != "Comparable" {
		t.Fatalf("cmp.Or constraint = %q", got)
	}

	slicesPackage, err := importer.Default().Import("slices")
	if err != nil {
		t.Fatal(err)
	}
	clone := slicesPackage.Scope().Lookup("Clone").(*gotypes.Func).Signature()
	if got := goType(clone.TypeParams().At(0).Constraint(), slicesPackage); got != "interface{~Slice<E>}" {
		t.Fatalf("slices.Clone constraint = %q", got)
	}
}

func TestCompareSnapshotsReportsPolicyDifferences(t *testing.T) {
	want := snapshot{Packages: []apiPackage{{Path: "complete", Complete: true, Declarations: []apiDeclaration{{Kind: "func", Name: "Keep", Type: "function()"}, {Kind: "var", Name: "Missing", Type: "Int"}}}, {Path: "subset", Declarations: []apiDeclaration{{Kind: "func", Name: "Keep", Type: "function(Int)"}}}}}
	got := snapshot{Packages: []apiPackage{{Path: "complete", Complete: true, Declarations: []apiDeclaration{{Kind: "func", Name: "Keep", Type: "function(String)"}, {Kind: "func", Name: "Extra", Type: "function()"}}}, {Path: "subset", Declarations: []apiDeclaration{{Kind: "func", Name: "Keep", Type: "function(Int)"}, {Kind: "var", Name: "Extra", Type: "Int"}}}}}
	differences := compareSnapshots(want, got)
	codes := make(map[string]bool, len(differences))
	for _, difference := range differences {
		codes[difference.Package+":"+difference.Name+":"+difference.Code] = true
	}
	for _, key := range []string{"complete:Keep:mismatch", "complete:Missing:missing", "complete:Extra:extra", "subset:Extra:extra"} {
		if !codes[key] {
			t.Fatalf("missing difference %q in %+v", key, differences)
		}
	}
}

func TestCompareSnapshotsReportsMissingPackagesRegardlessOfPolicy(t *testing.T) {
	want := snapshot{Packages: []apiPackage{{Path: "complete", Complete: true}, {Path: "subset"}}}
	differences := compareSnapshots(want, snapshot{})
	if len(differences) != 2 {
		t.Fatalf("missing package differences = %+v", differences)
	}
	for i, path := range []string{"complete", "subset"} {
		if difference := differences[i]; difference.Package != path || difference.Code != "missing_package" {
			t.Fatalf("difference[%d] = %+v, want package %q with missing_package", i, difference, path)
		}
	}
}

func TestDeclarationMatchesSubsetMembers(t *testing.T) {
	expected := apiDeclaration{Kind: "type", Name: "T", Type: "example.T", Methods: []apiField{{Name: "First", Type: "function()"}, {Name: "Second", Type: "function(Int)"}}}
	actual := apiDeclaration{Kind: "type", Name: "T", Type: "example.T", Methods: []apiField{{Name: "Second", Type: "function(Int)"}}}
	if !declarationMatches(expected, actual, false) {
		t.Fatal("subset should accept an implemented method subset")
	}
	if declarationMatches(expected, actual, true) {
		t.Fatal("complete package should require the full method set")
	}
	actual.Methods[0].Type = "function(String)"
	if declarationMatches(expected, actual, false) {
		t.Fatal("subset accepted a mismatched method signature")
	}
}

func TestTrackedDeclarationKeepsTestingAPIOutsideRunnerProtocol(t *testing.T) {
	for _, declaration := range []struct{ kind, name string }{{"func", "Main"}, {"type", "Report"}, {"type", "Result"}} {
		if trackedDeclaration("testing", declaration.kind, declaration.name) {
			t.Fatalf("testing.%s is part of the MiniGo runner protocol", declaration.name)
		}
	}
	if !trackedDeclaration("testing", "type", "T") || !trackedDeclaration("cmp", "func", "Or") {
		t.Fatal("ordinary standard-library declarations must remain tracked")
	}
}

func TestSnapshotEncodingIsDeterministicAndRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.gz"), filepath.Join(dir, "second.gz")
	value := snapshot{Schema: schema, Version: version, GoVersion: goVersion, Packages: []apiPackage{{Path: "errors"}}}
	if err := writeSnapshot(first, value); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := writeSnapshot(second, value); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadFile(first)
	right, _ := os.ReadFile(second)
	if !bytes.Equal(left, right) {
		t.Fatal("deterministic snapshots differ")
	}

	var decoded snapshot
	reader, err := gzip.NewReader(bytes.NewReader(left))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(reader).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	decoded.GoVersion = "go0"
	if err := writeSnapshot(first, decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshot(first); err == nil {
		t.Fatal("wrong Go version was accepted")
	}
}
