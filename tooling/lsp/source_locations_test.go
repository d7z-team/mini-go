package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/d7z-team/mini-go/compiler/workspace"
	"go.lsp.dev/uri"
)

func TestRegisteredLibraryLocationsRemainReadOnly(t *testing.T) {
	app, library := t.TempDir(), t.TempDir()
	for _, item := range []struct{ root, source string }{{app, "package app\nimport \"rules\"\nfunc Value() int { return rules.Value() }\n"}, {library, "package rules\nfunc Value() int { return 42 }\n"}} {
		if err := os.WriteFile(filepath.Join(item.root, "value.mgo"), []byte(item.source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := workspace.LoadSources(t.Context(), app, "app", []workspace.DirectorySource{{Module: "rules", Directory: library}})
	if err != nil {
		t.Fatal(err)
	}
	config := Workspace{RootPath: loaded.Root, ModulePath: loaded.ModulePath, Sources: loaded.Sources, Documents: loaded.Documents, Locations: loaded.Locations}
	filename := filepath.Join(library, "value.mgo")
	canonicalFile, err := filepath.EvalSymlinks(filename)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.documentURI("rules", "value.mgo"); got != DocumentURI(uri.File(canonicalFile)) {
		t.Fatalf("URI: %q", got)
	}
	server := NewProtocolServer(nil)
	server.workspace = config
	if _, err := server.identity(DocumentURI(uri.File(filename))); err == nil {
		t.Fatal("library edit accepted")
	}
	identity, err := server.identity(DocumentURI(uri.File(filepath.Join(app, "value.mgo"))))
	if err != nil || identity.ModulePath != "app" {
		t.Fatalf("app identity: %+v %v", identity, err)
	}
}
