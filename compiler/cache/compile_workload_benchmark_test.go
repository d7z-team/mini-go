package cache_test

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func BenchmarkCompileCacheRequests(b *testing.B) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "sample", Files: []source.File{{
		Path: "main.mgo", Text: "package sample\nfunc Double(x int) int { return x+x }\nfunc Answer() int { return Double(21) }",
	}}}})
	if err != nil {
		b.Fatal(err)
	}
	for _, backend := range []string{"transient", "serialized"} {
		for _, mode := range []string{"cold", "warm", "verify"} {
			b.Run(backend+"/"+mode, func(b *testing.B) {
				request := compiler.Request{Sources: sources, Root: "sample"}
				var transient *cache.TransientCache
				if backend == "transient" {
					transient = cache.NewTransient(cache.TransientConfig{})
					b.Cleanup(transient.Close)
					request.Cache = transient
				} else {
					request.Cache = cache.New(cache.NewMemoryBackend())
				}
				initial, err := compiler.Compile(request)
				if err != nil || !initial.OK() {
					b.Fatalf("compile: %v %+v", err, initial.Diagnostics)
				}
				request.CacheVerify = mode == "verify"
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if mode == "cold" {
						if transient != nil {
							transient.Clear()
						} else {
							request.Cache = cache.New(cache.NewMemoryBackend())
						}
					}
					result, err := compiler.Compile(request)
					if err != nil || !result.OK() || result.ArtifactHashes["sample"] != initial.ArtifactHashes["sample"] {
						b.Fatalf("compile changed result: %v %+v", err, result.Diagnostics)
					}
				}
			})
		}
	}
}
