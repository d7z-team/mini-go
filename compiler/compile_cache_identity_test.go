package compiler

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestCompilerLimitsCacheIdentity(t *testing.T) {
	defaults := normalizeCompilerLimits(Limits{})
	if defaults.cacheHash() != (Limits{}).cacheHash() {
		t.Fatal("default limits have different identities")
	}
	for i := 0; i < reflect.TypeFor[Limits]().NumField(); i++ {
		changed := defaults
		field := reflect.ValueOf(&changed).Elem().Field(i)
		field.SetInt(field.Int() - 1)
		if changed.cacheHash() == defaults.cacheHash() {
			t.Fatalf("limit %s did not affect identity", reflect.TypeFor[Limits]().Field(i).Name)
		}
	}
}

func TestCompileCacheActionUsesStructuredPackageIdentity(t *testing.T) {
	actionFor := func(namespace string) (string, string) {
		header := workspace.PackageHeader{
			Source: workspace.SourcePackage{
				ID:         workspace.PackageID{Namespace: namespace, Path: "main"},
				ModulePath: "example/main",
			},
			Package: "main",
		}
		action, key, err := workspaceCacheAction(header, nil, OptimizationDefault, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		return action.PackageID, key
	}

	firstID, firstKey := actionFor("workspace:first")
	secondID, secondKey := actionFor("workspace:second")
	if firstID != "workspace:first::main" || secondID != "workspace:second::main" {
		t.Fatalf("compile action package identities = %q, %q", firstID, secondID)
	}
	if firstKey == secondKey {
		t.Fatal("distinct package namespaces produced the same compile action")
	}
}
