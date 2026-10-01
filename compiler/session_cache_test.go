package compiler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileValidatesCustomCacheArtifacts(t *testing.T) {
	shared := cache.NewTransient(cache.TransientConfig{})
	defer shared.Close()
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "sample", Files: []source.File{{Path: "main.mgo", Text: "package sample\nfunc Answer() int { return 42 }"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Sources: sources, Root: "sample", Cache: shared}
	first, err := Compile(request)
	if err != nil || !first.OK() {
		t.Fatalf("initial compile: %v %+v", err, first.Diagnostics)
	}
	request.Cache = corruptArtifactCache{Cache: shared}
	result, err := Compile(request)
	if err != nil || !result.OK() {
		t.Fatalf("rebuild after corrupt cache hit: %v %+v", err, result.Diagnostics)
	}
	artifact := result.Artifacts["sample"]
	if err := ir.ValidateArtifact(&artifact); err != nil {
		t.Fatalf("custom cache bypassed artifact validation: %v", err)
	}
	if result.Stats.PackageCacheHits != 0 || result.Stats.PackagesCompiled != 1 {
		t.Fatalf("invalid cache entry was not rebuilt: %+v", result.Stats)
	}
}

type corruptArtifactCache struct{ cache.Cache }

func TestSessionCacheRefillOwnsNestedValues(t *testing.T) {
	shared := cache.NewTransient(cache.TransientConfig{})
	defer shared.Close()
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "sample", Files: []source.File{{Path: "main.mgo", Text: "package sample\nfunc Answer() int { return 42 }"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Compile(Request{Sources: sources, Root: "sample"})
	if err != nil || !result.OK() {
		t.Fatalf("compile: %v %+v", err, result.Diagnostics)
	}
	sealed, err := cache.SealArtifact(result.Artifacts["sample"])
	if err != nil {
		t.Fatal(err)
	}
	action := cache.NewCompileAction(ir.CompilerIdentity, result.Target, "sample", "sample", nil, nil)
	if _, err := shared.StoreCompile(action, sealed, result.PackageSymbols["sample"], result.ExportData["sample"]); err != nil {
		t.Fatal(err)
	}
	session := newSessionCache(shared, cache.TransientConfig{})
	defer session.Close()
	lookup, err := session.LookupCompile(action)
	if err != nil || !lookup.Hit {
		t.Fatalf("shared refill: %+v %v", lookup, err)
	}
	lookup.Artifact.Functions[0].Code.Instructions[0].Op = ir.Opcode(65535)
	lookup.Symbols.Functions[0].ID = "changed"
	shared.Clear()
	local, err := session.LookupCompile(action)
	if err != nil || !local.Hit {
		t.Fatalf("local hit: %+v %v", local, err)
	}
	hash, err := ir.Hash(&local.Artifact)
	if err != nil || hash != sealed.Hash() {
		t.Fatalf("refill leaked caller mutation: %s %v", hash, err)
	}
	if err := ir.ValidatePackageSymbols(&local.Artifact, hash, &local.Symbols); err != nil {
		t.Fatal(err)
	}
}

func (c corruptArtifactCache) LookupCompile(action cache.Action) (cache.Lookup, error) {
	lookup, err := c.Cache.LookupCompile(action)
	if err == nil && lookup.Hit {
		lookup.Artifact.Functions[0].Code.Instructions[0].Op = ir.Opcode(65535)
	}
	return lookup, err
}

func TestCompilerBorrowsStructuredCacheAcrossSessions(t *testing.T) {
	shared := cache.NewTransient(cache.TransientConfig{MaxEntries: 8, MaxBytes: 1 << 20})
	defer shared.Close()
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "sample", Files: []source.File{{Path: "main.mgo", Text: "package sample\nfunc Answer() int { return 42 }"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		compiler, err := New(Options{Sources: sources, Cache: shared})
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := compiler.Prepare("sample", []EntryPoint{{Name: "default", ModulePath: "sample", Function: "Answer"}})
		if closeErr := compiler.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || !prepared.Checked.OK() || prepared.Image == nil {
			t.Fatalf("prepare: %v %+v", err, prepared.Checked.Diagnostics)
		}
		if pass == 1 && shared.Stats().Hits == 0 {
			t.Fatal("closed compiler lost caller-owned cached package")
		}
		if stats := shared.Stats(); stats.Entries == 0 || stats.Bytes == 0 {
			t.Fatalf("shared cache released with compiler: %+v", stats)
		}
	}
	shared.Close()
	if stats := shared.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("cache owner close retained entries: %+v", stats)
	}
}

func TestSessionCacheCancellationReleasesLocalLock(t *testing.T) {
	shared := cache.New(cache.NewMemoryBackend())
	session := newSessionCache(shared, cache.TransientConfig{})
	defer session.Close()
	action := cache.NewCompileAction(Identity(), target.Target{}, "example/data", "data", nil, nil)
	release, err := shared.LockCompile(t.Context(), action)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	unlock, err := session.LockCompile(ctx, action)
	release()
	if unlock != nil {
		unlock()
		t.Fatal("acquired held shared lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", err)
	}
	retryCtx, retryCancel := context.WithTimeout(t.Context(), time.Second)
	defer retryCancel()
	unlock, err = session.LockCompile(retryCtx, action)
	if err != nil {
		t.Fatalf("local lock retained after cancel: %v", err)
	}
	unlock()
}

func TestSessionCacheReleasesCompileLockAfterStore(t *testing.T) {
	shared := cache.New(cache.NewMemoryBackend())
	session := newSessionCache(shared, cache.TransientConfig{})
	action := cache.NewCompileAction(Identity(), target.Target{}, "example/data", "data", nil, nil)
	release, err := session.LockCompile(t.Context(), action)
	if err != nil {
		t.Fatal(err)
	}
	artifact := ir.Artifact{
		Format: ir.Format, Version: ir.CurrentVersion,
		Module:    ir.Module{Path: "example/data", Package: "data"},
		OpcodeSet: ir.OpcodeSet,
	}
	data, err := cache.FromArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifactHash, err := ir.Hash(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	symbols := ir.PackageSymbols{ModulePath: artifact.Module.Path, CodeHash: artifactHash}
	sealed, err := cache.SealArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.StoreCompile(action, sealed, symbols, data); err != nil {
		t.Fatal(err)
	}
	release()
	second, err := session.LockCompile(t.Context(), action)
	if err != nil {
		t.Fatal(err)
	}
	second()
}

func TestSessionCacheRejectsInvalidActions(t *testing.T) {
	session := newSessionCache(nil, cache.TransientConfig{})
	defer session.Close()
	invalidTarget := target.Target{Tags: []string{"invalid tag"}}
	if _, err := session.LookupCompile(cache.Action{Target: invalidTarget}); err == nil {
		t.Fatal("invalid compile action accepted")
	}
	if _, err := session.LookupCompileManifest(cache.Action{Target: invalidTarget}); err == nil {
		t.Fatal("invalid manifest action accepted")
	}
	if _, err := session.LookupPrepare(cache.PrepareAction{Target: invalidTarget}); err == nil {
		t.Fatal("invalid prepare action accepted")
	}
}

func BenchmarkSessionCacheManifestMiss(b *testing.B) {
	session := newSessionCache(nil, cache.TransientConfig{})
	defer session.Close()
	action := cache.NewCompileAction(Identity(), target.Target{}, "example/data", "data", nil, nil)
	b.ReportAllocs()
	for b.Loop() {
		lookup, err := session.LookupCompileManifest(action)
		if err != nil || lookup.Hit {
			b.Fatalf("lookup = %v, %v", lookup, err)
		}
	}
}
