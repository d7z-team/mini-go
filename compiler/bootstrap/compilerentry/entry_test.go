package compilerentry

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileRejectsUnknownAndTrailingRequestData(t *testing.T) {
	valid := fmt.Sprintf(`{"Format":"mini-go-compiler-service","Version":%d,"Operation":"check","Root":"example/main","Packages":[]}`, ServiceVersion)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "unknown field", input: strings.TrimSuffix(valid, "}") + `,"Unknown":true}`, want: "invalid compiler request JSON"},
		{name: "trailing value", input: valid + ` {}`, want: "compiler request contains trailing JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response Response
			if err := json.Unmarshal(Compile([]byte(test.input)), &response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(response.Error, test.want) {
				t.Fatalf("Compile error = %q, want %q", response.Error, test.want)
			}
		})
	}
}

func TestExecuteSelectsRequestBuildTags(t *testing.T) {
	request := Request{
		Format: ServiceFormat, Version: ServiceVersion, Operation: OperationPrepare, Root: "example/main",
		Packages: []Package{{Namespace: "module:example", PackagePath: "main", ModulePath: "example/main", Files: []File{
			{Path: "main.mgo", Text: "package main\nfunc Main() string { return mode() }\n"},
			{Path: "mode_default.mgo", Text: "//go:build !feature\n\npackage main\nfunc mode() string { return \"default\" }\n"},
			{Path: "mode_feature.mgo", Text: "//go:build feature\n\npackage main\nfunc mode() string { return \"feature\" }\n"},
		}}},
		EntryPoints: []compiler.EntryPoint{{Name: "main", ModulePath: "example/main", Function: "Main"}},
	}
	standard := Execute(request)
	if standard.Error != "" || standard.Image == nil || len(standard.Diagnostics) != 0 {
		t.Fatalf("default response = %#v", standard)
	}
	request.Tags = []string{"feature"}
	feature := Execute(request)
	if feature.Error != "" || feature.Image == nil || len(feature.Diagnostics) != 0 {
		t.Fatalf("feature response = %#v", feature)
	}
	if standard.Image.Hash == feature.Image.Hash {
		t.Fatal("build tags did not change the prepared image")
	}
	request.Tags = []string{"bad tag"}
	if response := Execute(request); !strings.Contains(response.Error, "invalid build tag") {
		t.Fatalf("invalid tag response = %#v", response)
	}
}

func TestExecuteValidatesSourceSuffixBeforeCompilation(t *testing.T) {
	for _, placement := range []string{"production", "test", "unreferenced package"} {
		t.Run(placement, func(t *testing.T) {
			request := Request{
				Format: ServiceFormat, Version: ServiceVersion, Operation: OperationPrepare, Root: "example/main",
				Packages: []Package{{Namespace: "module:example", PackagePath: "main", ModulePath: "example/main", Files: []File{{
					Path: "main.mgo", Text: "package main\nfunc main() {}\n",
				}}}},
			}
			invalid := File{Path: "host.go", Text: "package main\n"}
			switch placement {
			case "production":
				request.Packages[0].Files = append(request.Packages[0].Files, invalid)
			case "test":
				invalid.Path = "host_test.go"
				request.Packages[0].TestFiles = []File{invalid}
			case "unreferenced package":
				request.Packages = append(request.Packages, Package{Namespace: "module:example", PackagePath: "unused", ModulePath: "example/unused", Files: []File{invalid}})
			}
			response := Execute(request)
			if !strings.Contains(response.Error, "must use .mgo") || response.Image != nil {
				t.Fatalf("invalid input response = %#v", response)
			}
		})
	}
}

func TestExecuteAppliesOptimizationOnlyToCodeGeneration(t *testing.T) {
	request := Request{
		Format: ServiceFormat, Version: ServiceVersion, Operation: OperationCheck, Root: "example/main",
		Optimization: compiler.OptimizationFull + 1,
		Packages: []Package{{Namespace: "module:example", PackagePath: "main", ModulePath: "example/main", Files: []File{{
			Path: "main.mgo", Text: "package main\nfunc main() {}\n",
		}}}},
	}
	if response := Execute(request); response.Error != "" || len(response.Diagnostics) != 0 {
		t.Fatalf("check response = %#v", response)
	}
	request.Operation = OperationPrepare
	if response := Execute(request); !strings.Contains(response.Error, "unsupported compiler optimization level") {
		t.Fatalf("prepare response = %#v", response)
	}
	request.Optimization = compiler.OptimizationDefault
}

func TestServiceReusesBoundedStructuredCache(t *testing.T) {
	service := NewService(cache.TransientConfig{MaxEntries: 8, MaxBytes: 8 << 20})
	request := Request{
		Format: ServiceFormat, Version: ServiceVersion, Operation: OperationPrepare, Root: "example/main",
		Packages: []Package{{Namespace: "module:example", PackagePath: "main", ModulePath: "example/main", Files: []File{{
			Path: "main.mgo", Text: "package main\nfunc Main() int { return 42 }\n",
		}}}},
		EntryPoints: []compiler.EntryPoint{{Name: "main", ModulePath: "example/main", Function: "Main"}},
	}
	first := service.Execute(request)
	if first.Error != "" || first.Image == nil || first.Stats.PackagesCompiled != 1 || first.CacheStats.Stores == 0 {
		t.Fatalf("cold response = %#v", first)
	}
	second := service.Execute(request)
	if second.Error != "" || second.Image == nil || second.Image.Hash != first.Image.Hash || second.Stats.PrepareCacheHits != 1 || second.Stats.ImagesLinked != 0 || second.CacheStats.Hits == 0 {
		t.Fatalf("warm response = %#v", second)
	}
	optimized := request
	optimized.Optimization = compiler.OptimizationDefault
	third := service.Execute(optimized)
	if third.Error != "" || third.Image == nil || third.Stats.PackageCacheHits != 0 || third.Stats.PackagesCompiled != 1 {
		t.Fatalf("cold optimized response = %#v", third)
	}
	fourth := service.Execute(optimized)
	if fourth.Error != "" || fourth.Image == nil || fourth.Stats.PrepareCacheHits != 1 || fourth.Stats.ImagesLinked != 0 {
		t.Fatalf("warm optimized response = %#v", fourth)
	}
	withSymbols := request
	withSymbols.Symbols = true
	symbolResponse := service.Execute(withSymbols)
	if symbolResponse.Error != "" || symbolResponse.Image == nil || symbolResponse.Symbols == nil || symbolResponse.Image.Hash != first.Image.Hash || symbolResponse.Stats.PackageCacheHits != 1 {
		t.Fatalf("symbol response = %#v", symbolResponse)
	}
	if err := ir.ValidateProgramSymbols(symbolResponse.Image, symbolResponse.Symbols); err != nil {
		t.Fatal(err)
	}
	service.Close()
	if response := service.Execute(request); response.Error != "compiler service is closed" {
		t.Fatalf("closed response = %#v", response)
	}
}

func TestServiceReusesAnalysisOnlyForMatchingSourceAndTarget(t *testing.T) {
	service := NewService(cache.TransientConfig{})
	defer service.Close()
	request := Request{
		Operation: OperationCheck, Root: "sample",
		Packages: []Package{{Namespace: "module:sample", ModulePath: "sample", Files: []File{{
			Path: "main.mgo", Text: "package sample\nfunc Main() int { return 41 }\n",
		}}}},
	}
	first := service.Execute(request)
	if first.Error != "" || len(first.Diagnostics) != 0 || first.Stats.PackagesParsed != 1 {
		t.Fatalf("first check = %#v", first)
	}
	second := service.Execute(request)
	if second.Error != "" || len(second.Diagnostics) != 0 || second.Stats.PackageCacheHits != 1 || second.Stats.PackagesParsed != 0 {
		t.Fatalf("repeated check = %#v", second)
	}
	request.Packages[0].Files[0].Text = "package sample\nfunc Main() int { return 42 }\n"
	changed := service.Execute(request)
	if changed.Error != "" || len(changed.Diagnostics) != 0 || changed.Stats.PackagesParsed != 1 {
		t.Fatalf("changed source = %#v", changed)
	}
	request.Tags = []string{"feature"}
	retargeted := service.Execute(request)
	if retargeted.Error != "" || len(retargeted.Diagnostics) != 0 || retargeted.Stats.PackagesParsed != 1 {
		t.Fatalf("changed target = %#v", retargeted)
	}
	request.Operation = OperationPrepare
	request.EntryPoints = []compiler.EntryPoint{{Name: "main", ModulePath: "sample", Function: "Main"}}
	warm := service.Execute(request)
	if warm.Error != "" || warm.Image == nil || len(warm.Diagnostics) != 0 || warm.Stats.PackagesParsed != 0 || warm.Stats.PackagesAnalyzed != 0 {
		t.Fatalf("prepare after check = %#v", warm)
	}
	coldService := NewService(cache.TransientConfig{})
	defer coldService.Close()
	cold := coldService.Execute(request)
	if cold.Error != "" || cold.Image == nil || cold.Image.Hash != warm.Image.Hash || cold.Stats.PackagesParsed != 1 {
		t.Fatalf("cold prepare = %#v", cold)
	}
	request.Packages[0].Files[0].Text = "package sample\nfunc Main() int { return 43 }\n"
	invalidated := service.Execute(request)
	if invalidated.Error != "" || invalidated.Image == nil || invalidated.Stats.PackagesParsed != 1 || invalidated.Image.Hash == warm.Image.Hash {
		t.Fatalf("changed source prepare = %#v", invalidated)
	}
}

func TestServiceCoreSnapshotSurvivesRequestsAndIsReleasedOnClose(t *testing.T) {
	service := NewService(cache.TransientConfig{})
	defer service.Close()
	request := Request{Operation: OperationCheck, Root: "sample", Packages: []Package{{Namespace: "module:sample", ModulePath: "sample", Files: []File{{Path: "main.mgo", Text: "package sample\nfunc Main() int { return 1 }\n"}}}}}
	if response := service.Execute(request); response.Error != "" || len(response.Diagnostics) != 0 {
		t.Fatalf("initial: %#v", response)
	}
	snapshot := service.library
	request.Packages[0].Files[0].Text = "package sample\nfunc Main() int { return 2 }\n"
	if response := service.Execute(request); response.Error != "" || len(response.Diagnostics) != 0 {
		t.Fatalf("edited: %#v", response)
	}
	if service.library != snapshot {
		t.Fatal("request rebuilt immutable core snapshot")
	}
	service.compilerID = "previous compiler"
	if response := service.Execute(request); response.Error != "" || len(response.Diagnostics) != 0 {
		t.Fatalf("identity change: %#v", response)
	}
	if service.library == snapshot {
		t.Fatal("compiler identity retained previous core snapshot")
	}
	service.Close()
	if service.library != nil || service.analysis != nil {
		t.Fatal("closed service retained source or analysis")
	}
}
