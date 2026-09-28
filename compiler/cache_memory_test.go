package compiler_test

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestTransientCacheBoundsGenericTemplatesWithoutEvictingSmallPackages(t *testing.T) {
	store := cache.NewTransient(cache.TransientConfig{MaxEntries: 8, MaxBytes: 32 << 10})
	compile := func(path, text string) compiler.Result {
		sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: path, Files: []source.File{{Path: "main.mgo", Text: text}}}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := compiler.Compile(compiler.Request{Root: path, Sources: sources, Cache: store})
		if err != nil || !result.OK() {
			t.Fatalf("compile: %v %v", err, result.Diagnostics)
		}
		return result
	}
	small := "package small\nfunc Value() int { return 42 }"
	compile("small", small)
	before := store.Stats()
	large := "package large\nfunc Keep[T any](v T) T { _ = \"" + strings.Repeat("x", 128<<10) + "\"; return v }"
	for range 2 {
		if result := compile("large", large); result.Stats.PackageCacheHits != 0 {
			t.Fatal("oversized generic template remained cached")
		}
	}
	after := store.Stats()
	if after.Bytes != before.Bytes || after.Entries != before.Entries || after.Evictions != before.Evictions {
		t.Fatalf("large input disturbed small cache: before=%+v after=%+v", before, after)
	}
	if result := compile("small", small); result.Stats.PackageCacheHits != 1 {
		t.Fatal("small package did not remain reusable")
	}
	store.Close()
	if stats := store.Stats(); stats.Bytes != 0 || stats.Entries != 0 {
		t.Fatalf("close: %+v", stats)
	}
}
