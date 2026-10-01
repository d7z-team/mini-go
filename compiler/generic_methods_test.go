package compiler

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func TestGenericMethodConstraintsAndReceiverRules(t *testing.T) {
	for _, tc := range []struct{ name, source, code string }{
		{"constraint", `type S struct{};func (S) M[T ~int](v T) T{return v};func Main(){S{}.M("bad")}`, "semantic.const.representable"},
		{"duplicate", `type S struct{};func (S) M[T any](v T) T{return v};func (S) M(v int) int{return v}`, "semantic.method.duplicate"},
		{"pointer path", `type S struct{X int};type O struct{*S};func Main(){_ = O{X:1}}`, "semantic.composite.embedded_pointer"},
		{"overlap", `type S struct{X int};type O struct{S};func Main(){_ = O{X:1,S:S{}}}`, "semantic.composite.overlap"},
		{"diamond", `type S struct{X int};type A struct{S};type B struct{S};type O struct{A;B};func Main(){_ = O{X:1}}`, "semantic.composite.ambiguous"},
		{"parameter redeclared", `type S[T any] struct{};func (S[T]) M[T any](v T) T{return v}`, "semantic.scope.duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := compileTestSource("example", "methods.mgo", "package p\n"+tc.source)
			if err != nil {
				t.Fatal(err)
			}
			requireCompileDiagnostic(t, compiled.Diagnostics, tc.code)
		})
	}
}

func TestGenericPromotedLiteralAndPartialMethodInference(t *testing.T) {
	compiled, err := compileTestSource("example", "methods.mgo", `package p
type Inner[T any] struct{ X T }
type Outer[T any] struct{ Inner[T] }
func (b Outer[T]) Map[U any,V any](x U,y V) V{return y}
type Number[T ~int] struct{Value T}
func (n Number[T]) Add[U ~int](value U) T{return n.Value+T(value)}
func (n Number[T]) Echo[Echo any](value Echo) Echo {return value}
func Main() int {
 b:=Outer[int]{X:1}
 var f func(int,string)string=b.Map[int]
 if f(2,"ok")!="ok" { panic("method") }
 if (Number[int]{Value:2}).Add(3)!=5 {panic("receiver constraint")}
 if (Number[int]{}).Echo("ok")!="ok" {panic("parameter scope")}
 return b.Inner.X
}`)
	if err != nil || !compiled.OK() {
		t.Fatalf("compile: %v %+v", err, compiled.Diagnostics)
	}
}

func TestGenericMethodConstraintEditInvalidatesCache(t *testing.T) {
	storage := cache.New(cache.NewMemoryBackend())
	var originalHash string
	for _, constraint := range []string{"~int", "~int64", "~int"} {
		sources, err := workspace.NewMemorySourceSet([]SourcePackage{
			{ModulePath: "lib", Files: []SourceFile{{Path: "lib.mgo", Text: "package lib\ntype Box struct{}\nfunc (Box) Echo[T " + constraint + "](x T) T {return x}"}}},
			{ModulePath: "main", Files: []SourceFile{{Path: "main.mgo", Text: "package main\nimport \"lib\"\nfunc Main() int {return lib.Box{}.Echo[int](1)}"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			result, err := Prepare(Request{Root: "main", Sources: sources, Cache: storage, EntryPoints: []EntryPoint{{Name: "main", Function: "Main"}}})
			if err != nil {
				t.Fatal(err)
			}
			if constraint == "~int64" {
				found := false
				for _, diagnostic := range result.Checked.Diagnostics {
					found = found || strings.Contains(string(diagnostic.Code), "constraint")
				}
				if result.Image != nil || !found {
					t.Fatalf("constraint edit accepted: %+v", result.Checked.Diagnostics)
				}
				continue
			}
			if !result.Checked.OK() || result.Image == nil {
				t.Fatalf("valid constraint: %+v", result.Checked.Diagnostics)
			}
			if originalHash != "" && result.Image.Hash != originalHash {
				t.Fatal("restored declaration did not restore image identity")
			}
			originalHash = result.Image.Hash
		}
	}
}
