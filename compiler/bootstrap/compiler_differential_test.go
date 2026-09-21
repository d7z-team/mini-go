package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
	"github.com/d7z-team/mini-go/compiler/cache"
)

func TestCompilerImageMatchesNativeCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("bootstrap compiler execution is an integration test")
	}
	image := buildCompilerImage(t)
	compilerInstance := instantiateCompilerImage(t, image)
	native := compilerentry.NewService(cache.TransientConfig{MaxEntries: 64, MaxBytes: 128 << 20})
	t.Cleanup(native.Close)

	for _, test := range compilerDifferentialCorpus() {
		t.Run(test.name, func(t *testing.T) {
			input, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			want := native.Execute(test.request)
			got := callCompilerImage(t, compilerInstance, input)
			compareCompilerResponse(t, got, want)
			if !test.warm {
				return
			}
			if got.Image == nil {
				t.Fatalf("valid corpus did not produce an image: error=%s diagnostics=%+v", got.Error, got.Diagnostics)
			}
			warm := callCompilerImage(t, compilerInstance, input)
			compareCompilerResponse(t, warm, want)
			if warm.Image == nil || got.Image == nil || warm.Image.Hash != got.Image.Hash {
				t.Fatalf("warm image changed: cold=%#v warm=%#v", got.Image, warm.Image)
			}
			if warm.Stats.PrepareCacheHits != 1 || warm.Stats.ImagesLinked != 0 || warm.CacheStats.Hits == 0 {
				t.Fatalf("warm request did not reuse prepared compiler state: stats=%#v cache=%#v", warm.Stats, warm.CacheStats)
			}
		})
	}
}

type compilerDifferentialCase struct {
	name    string
	request compilerentry.Request
	warm    bool
}

func compilerDifferentialCorpus() []compilerDifferentialCase {
	request := func(operation compilerentry.Operation, root string, packages []compilerentry.Package, entries ...compiler.EntryPoint) compilerentry.Request {
		return compilerentry.Request{
			Format: compilerentry.ServiceFormat, Version: compilerentry.ServiceVersion,
			Operation: operation, Root: root, Packages: packages, EntryPoints: entries,
			Optimization: compiler.OptimizationDefault,
		}
	}
	cases := []compilerDifferentialCase{
		{
			name: "language features",
			request: request(compilerentry.OperationPrepare, "corpus/main", []compilerentry.Package{{
				Namespace: "module:corpus", PackagePath: "main", ModulePath: "corpus/main",
				Files: []compilerentry.File{{Path: "main.mgo", Text: `package main

type Pair struct { Left int; Right int }
func (p Pair) Total() int { return p.Left + p.Right }
func Identity[T any](value T) T { return value }

func Result() int {
	values := []int{1, 2, 3}
	lookup := map[string]int{"left": values[0], "right": values[2]}
	pair := Identity(Pair{Left: lookup["left"], Right: lookup["right"]})
	total := 0
	for _, value := range values { total += value }
	switch pair.Total() {
	case 4:
		return total + pair.Total()
	default:
		return 0
	}
}
`}},
			}}, compiler.EntryPoint{Name: "result", ModulePath: "corpus/main", Function: "Result"}),
			warm: true,
		},
		{
			name: "package graph",
			request: request(compilerentry.OperationPrepare, "corpus/app", []compilerentry.Package{
				{
					Namespace: "module:corpus", PackagePath: "lib", ModulePath: "corpus/lib",
					Files: []compilerentry.File{{Path: "lib.mgo", Text: "package lib\nfunc Value() int { return 21 }\n"}},
				},
				{
					Namespace: "module:corpus", PackagePath: "app", ModulePath: "corpus/app",
					Files: []compilerentry.File{{Path: "main.mgo", Text: "package main\nimport \"corpus/lib\"\nfunc Result() int { return lib.Value() * 2 }\n"}},
				},
			}, compiler.EntryPoint{Name: "result", ModulePath: "corpus/app", Function: "Result"}),
			warm: true,
		},
		{
			name: "semantic diagnostic",
			request: request(compilerentry.OperationCheck, "corpus/broken", []compilerentry.Package{{
				Namespace: "module:corpus", PackagePath: "broken", ModulePath: "corpus/broken",
				Files: []compilerentry.File{{Path: "broken.mgo", Text: "package broken\nfunc Value() int { return missing }\n"}},
			}}),
		},
		{
			name: "source suffix validation",
			request: request(compilerentry.OperationPrepare, "corpus/main", []compilerentry.Package{{
				Namespace: "module:corpus", PackagePath: "main", ModulePath: "corpus/main",
				Files: []compilerentry.File{{Path: "main.go", Text: "package main\nfunc main() {}\n"}},
			}}),
		},
		{
			name: "missing imported member",
			request: request(compilerentry.OperationPrepare, "corpus/app", []compilerentry.Package{
				{Namespace: "module:corpus", PackagePath: "lib", ModulePath: "corpus/lib", Files: []compilerentry.File{{Path: "lib.mgo", Text: "package lib\n"}}},
				{Namespace: "module:corpus", PackagePath: "app", ModulePath: "corpus/app", Files: []compilerentry.File{{Path: "main.mgo", Text: "package main\nimport \"corpus/lib\"\nfunc main() { lib.Missing() }\n"}}},
			}),
		},
	}
	cases[0].request.Symbols = false
	cases[1].request.Symbols = true
	for _, generated := range generatedCompilerCorpus {
		cases = append(cases, compilerDifferentialCase{
			name:    "generated " + generated.name,
			request: generatedCompilerRequest(generated.data, compilerentry.OperationPrepare),
		})
	}
	return cases
}

func compareCompilerResponse(t *testing.T, got, want compilerentry.Response) {
	t.Helper()
	if got.Format != want.Format || got.Version != want.Version || got.CompilerID != want.CompilerID || got.Error != want.Error {
		t.Fatalf("compiler envelope differs: got=%#v want=%#v", got, want)
	}
	if !reflect.DeepEqual(got.Diagnostics, want.Diagnostics) {
		t.Fatalf("compiler diagnostics differ: got=%#v want=%#v", got.Diagnostics, want.Diagnostics)
	}
	if !reflect.DeepEqual(got.Tests, want.Tests) {
		t.Fatalf("compiler test manifest differs: got=%#v want=%#v", got.Tests, want.Tests)
	}
	if (got.Image == nil) != (want.Image == nil) {
		t.Fatalf("compiler image presence differs: got=%#v want=%#v", got.Image, want.Image)
	}
	if got.Image != nil && got.Image.Hash != want.Image.Hash {
		for modulePath, wantArchive := range want.Image.Packages {
			gotArchive, ok := got.Image.Packages[modulePath]
			if !ok || gotArchive.ArtifactHash != wantArchive.ArtifactHash {
				t.Logf("package %q: got artifact=%q, want=%q, present=%t",
					modulePath, gotArchive.ArtifactHash, wantArchive.ArtifactHash, ok)
				if ok {
					limit := len(gotArchive.Artifact)
					if len(wantArchive.Artifact) < limit {
						limit = len(wantArchive.Artifact)
					}
					for offset := 0; offset < limit; offset++ {
						if gotArchive.Artifact[offset] == wantArchive.Artifact[offset] {
							continue
						}
						end := offset + 80
						if end > limit {
							end = limit
						}
						t.Logf("package %q first artifact difference at %d: got=%q want=%q",
							modulePath, offset, gotArchive.Artifact[offset:end], wantArchive.Artifact[offset:end])
						break
					}
				}
			}
		}
		t.Fatalf("compiler image hash differs: got=%q want=%q", got.Image.Hash, want.Image.Hash)
	}
	if got.Image != nil {
		gotJSON, err := json.Marshal(got.Image)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want.Image)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("compiler image bytes differ despite matching identity: got=%d bytes want=%d bytes", len(gotJSON), len(wantJSON))
		}
	}
	if (got.Symbols == nil) != (want.Symbols == nil) {
		t.Fatalf("compiler symbol presence differs: got=%#v want=%#v", got.Symbols, want.Symbols)
	}
	if got.Symbols != nil {
		if got.Symbols.Hash != want.Symbols.Hash {
			t.Fatalf("compiler symbol hash differs: got=%q want=%q", got.Symbols.Hash, want.Symbols.Hash)
		}
		gotJSON, err := json.Marshal(got.Symbols)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want.Symbols)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("compiler symbol bytes differ despite matching identity: got=%d bytes want=%d bytes", len(gotJSON), len(wantJSON))
		}
	}
}
