package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectorySourcesIndexOwnersAndSnapshot(t *testing.T) {
	app, dependency := t.TempDir(), t.TempDir()
	for _, root := range []string{app, dependency} {
		if err := os.Mkdir(filepath.Join(root, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "nested/value.mgo"), []byte("package nested\nconst Value = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := LoadSources(t.Context(), app, "app", []DirectorySource{{Module: "rules", Directory: dependency}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dependency, "nested/value.mgo")
	canonicalFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := loaded.Locations.Identity(file)
	if !ok || identity.ModulePath != "rules/nested" || identity.Path != "nested/value.mgo" {
		t.Fatalf("identity: %+v %v", identity, ok)
	}
	if got, ok := loaded.Locations.File(identity.ModulePath, identity.Path); !ok || got != canonicalFile {
		t.Fatalf("file: %q %v", got, ok)
	}
	if _, editable, _ := loaded.Documents.Package("rules/nested"); editable {
		t.Fatal("dependency is editable")
	}
	if err := os.WriteFile(file, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg, _, err := loaded.Sources.Package("rules/nested")
	if err != nil || pkg.Files[0].Text != "package nested\nconst Value = 1\n" {
		t.Fatalf("snapshot changed: %+v %v", pkg, err)
	}
	for _, source := range []DirectorySource{{Module: "other", Directory: app}, {Module: "other", Directory: filepath.Join(app, "nested")}, {Module: "app", Directory: dependency}} {
		if _, err := LoadSources(t.Context(), app, "app", []DirectorySource{source}); err == nil {
			t.Fatalf("ambiguous source accepted: %+v", source)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := LoadSources(canceled, app, "app", nil); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestSourcePrefixesRejectAmbiguousPaths(t *testing.T) {
	for _, prefix := range []string{"", "/root", "a/../b", "a//b", "a\\b", "a b", "a\x00b"} {
		if _, err := NewTreeSourceSet(prefix, nil); err == nil {
			t.Fatalf("invalid prefix accepted: %q", prefix)
		}
	}
	for _, prefix := range []string{"app", "company/rules", "rules/v2"} {
		if _, err := NewTreeSourceSet(prefix, nil); err != nil {
			t.Fatal(err)
		}
	}
}
