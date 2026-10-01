package cache_test

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileStoresPreserveNestedSnapshotOwnership(t *testing.T) {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: "sample", Files: []source.File{{
		Path: "main.mgo", Text: "package sample\ntype Point struct { X int }\nfunc Answer(x int) int { return x + 42 }",
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(compiler.Request{Sources: sources, Root: "sample"})
	if err != nil || !compiled.OK() {
		t.Fatalf("compile: %v %+v", err, compiled.Diagnostics)
	}
	mutations := map[string]func(*ir.Artifact){
		"instruction": func(a *ir.Artifact) { a.Functions[0].Code.Instructions[0].Op = ir.Opcode(65535) },
		"operand": func(a *ir.Artifact) {
			for i := range a.Functions[0].Code.Operands {
				if len(a.Functions[0].Code.Operands[i].Inputs) != 0 {
					a.Functions[0].Code.Operands[i].Inputs[0].Index = ^uint32(0)
					return
				}
			}
			t.Fatal("fixture has no input operand")
		},
		"constant": func(a *ir.Artifact) { a.Constants[0].Value[0] = '9' },
		"type":     func(a *ir.Artifact) { a.TypeTable.Nodes[0].ID = "changed" },
	}
	for _, backend := range []string{"transient", "memory", "disk"} {
		for _, boundary := range []string{"input", "copy", "lookup"} {
			for name, mutate := range mutations {
				t.Run(backend+"/"+boundary+"/"+name, func(t *testing.T) {
					var store cache.Cache
					switch backend {
					case "transient":
						local := cache.NewTransient(cache.TransientConfig{})
						t.Cleanup(local.Close)
						store = local
					case "memory":
						store = cache.New(cache.NewMemoryBackend())
					case "disk":
						store = cache.New(cache.NewDiskBackend(t.TempDir()))
					}
					artifact := ir.CloneArtifact(compiled.Artifacts["sample"])
					sealed, err := cache.SealArtifact(artifact)
					if err != nil {
						t.Fatal(err)
					}
					action := cache.NewCompileAction(ir.CompilerIdentity, compiled.Target, "sample", "sample", nil, nil)
					manifest, err := store.StoreCompile(action, sealed, compiled.PackageSymbols["sample"], compiled.ExportData["sample"])
					if err != nil {
						t.Fatal(err)
					}
					mutable := artifact
					if boundary == "copy" {
						mutable = sealed.Copy()
					}
					if boundary == "lookup" {
						lookup, err := store.LookupCompile(action)
						if err != nil || !lookup.Hit {
							t.Fatalf("lookup: %v %+v", err, lookup)
						}
						mutable = lookup.Artifact
					}
					mutate(&mutable)
					lookup, err := store.LookupCompile(action)
					if err != nil || !lookup.Hit {
						t.Fatalf("lookup after mutation: %v %+v", err, lookup)
					}
					hash, err := ir.Hash(&lookup.Artifact)
					if err != nil || hash != sealed.Hash() || manifest.ArtifactHash != hash || lookup.ArtifactHash != hash {
						t.Fatalf("snapshot identity changed: %s %s %v", hash, sealed.Hash(), err)
					}
					gotSymbols, err := ir.HashPackageSymbols(lookup.Symbols)
					if err != nil {
						t.Fatal(err)
					}
					wantSymbols, err := ir.HashPackageSymbols(compiled.PackageSymbols["sample"])
					if err != nil {
						t.Fatal(err)
					}
					if gotSymbols != wantSymbols || lookup.ExportHash != compiled.ExportHashes["sample"] {
						t.Fatalf("cached metadata changed: symbols=%s want=%s export=%s want=%s", gotSymbols, wantSymbols, lookup.ExportHash, compiled.ExportHashes["sample"])
					}
				})
			}
		}
	}
}
