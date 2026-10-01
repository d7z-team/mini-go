package integrations_test

import (
	"strings"
	"testing"

	minigo "github.com/d7z-team/mini-go"
	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
	miniruntime "github.com/d7z-team/mini-go/runtime"
)

func TestGenericMethodsAndInitializerContexts(t *testing.T) {
	engine := newIntegrationEngine(t, "execution/generic_methods", "example", target.Target{})
	for _, tc := range []struct {
		name string
		want int64
	}{
		{"PromotedOrder", 123},
		{"Contexts", 6},
		{"ImportedMethods", 13},
		{"CapturedReceiver", 5},
		{"DefinitionState", 6},
		{"PromotedMethod", 8},
		{"AliasMethods", 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 2 {
				program, result, err := engine.Compile("example", minigo.EntryPoint{Name: "result", Function: tc.name})
				if err != nil || !result.OK() {
					t.Fatalf("compile: %v %+v", err, result)
				}
				requireIntegrationInt(t, callIntegrationEntry(t, program, "result"), tc.want)
			}
		})
	}
}

func TestGenericMethodPatchPreservesDefinitionState(t *testing.T) {
	backend := cache.NewMemoryBackend()
	var programs []*miniruntime.Program
	for _, revision := range []struct{ helper, method, global string }{{"1", "0", "int"}, {"2", "0", "int"}, {"2", "100", "int"}, {"2", "100", "int64"}} {
		library := strings.NewReplacer("HELPER", revision.helper, "BONUS", revision.method, "GLOBAL", revision.global).Replace(`package lib
var count GLOBAL
func hidden(x int) int { count += GLOBAL(x); return int(count) + HELPER }
type Box struct { Value int }
func (b Box) Add[T ~int](x T) T { return T(hidden(int(x)) + b.Value + BONUS) }
`)
		sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{
			{ModulePath: "example", Files: []source.File{{Path: "main.mgo", Text: `package main
import "example/lib"
var saved func(int) int
func Main() int {
 if saved == nil { saved = lib.Box{Value:10}.Add[int] }
 return saved(1)
}`}}},
			{ModulePath: "example/lib", Files: []source.File{{Path: "lib.mgo", Text: library}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var imageHash string
		for range 2 {
			prepared, err := compiler.Prepare(compiler.Request{Root: "example", Sources: sources, Cache: cache.New(backend), EntryPoints: []compiler.EntryPoint{{Name: "run", Function: "Main"}}})
			if err != nil || prepared.Image == nil || !prepared.Checked.OK() {
				t.Fatalf("prepare: %v %+v", err, prepared.Checked.Diagnostics)
			}
			if imageHash != "" {
				if prepared.Image.Hash != imageHash {
					t.Fatal("warm cache differs")
				}
				continue
			}
			imageHash = prepared.Image.Hash
			program, err := miniruntime.LoadExecutionImage(*prepared.Image)
			if err != nil {
				t.Fatal(err)
			}
			programs = append(programs, program)
		}
	}
	instance, err := programs[0].Instantiate(t.Context(), miniruntime.InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	for index, want := range []int64{12, 14, 115} {
		if index > 0 {
			plan, err := instance.PreparePatch(t.Context(), programs[index])
			if err != nil {
				t.Fatal(err)
			}
			_, err = instance.ApplyPatch(plan)
			if err != nil {
				t.Fatal(err)
			}
		}
		result, err := instance.Call(t.Context(), "run")
		if err != nil {
			t.Fatal(err)
		}
		requireIntegrationInt(t, result.Values[0], want)
	}
	if plan, err := instance.PreparePatch(t.Context(), programs[3]); err == nil {
		plan.Close()
		t.Fatal("incompatible global layout accepted")
	}
	result, err := instance.Call(t.Context(), "run")
	if err != nil {
		t.Fatal(err)
	}
	requireIntegrationInt(t, result.Values[0], 116)
}
