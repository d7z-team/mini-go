package specialize_test

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/emit"
	"github.com/d7z-team/mini-go/compiler/lower"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/specialize"
)

func TestOrdinaryProgramMatchesFullSpecialization(t *testing.T) {
	for name, body := range map[string]string{
		"scalar":     `func Value(x int) int { if x > 0 { return x + 1 }; return 0 }`,
		"closure":    `func Value(x int) func() int { return func() int { x++; return x } }`,
		"method":     `type Counter struct { Value int }; func (c *Counter) Add(n int) { c.Value += n }`,
		"local_type": `func Value(x int) int { type Count int; var n Count = Count(x); return int(n) }`,
		"loop":       `func Value(s []int) int { n := 0; for _, x := range s { n += x }; return n }`,
	} {
		t.Run(name, func(t *testing.T) {
			parse := func() semantic.CheckedProgram {
				parsed := parser.ParseSource("example", "value.mgo", "package example\n"+body)
				if len(parsed.Diagnostics) != 0 {
					t.Fatal(parsed.Diagnostics)
				}
				checked := semantic.Check(parsed.Program)
				if len(checked.Info.Diagnostics) != 0 {
					t.Fatal(checked.Info.Diagnostics)
				}
				return checked
			}
			checked := parse()
			unused := map[string]cache.PackageData{"unrelated/generic": {GenericTemplates: []cache.GenericTemplate{{Kind: "function", Name: "Identity"}}}}
			if specialize.Required(checked, nil) || specialize.Required(checked, unused) {
				t.Fatal("ordinary package requires specialization")
			}
			fastHIR, diagnostics := lower.Lower(checked, lower.Options{})
			if len(diagnostics) != 0 {
				t.Fatal(diagnostics)
			}
			full, diagnostics, err := specialize.Apply(parse(), nil)
			if err != nil || len(diagnostics) != 0 {
				t.Fatalf("specialization: %v, %v", err, diagnostics)
			}
			fullHIR, diagnostics := lower.Lower(semantic.Check(full), lower.Options{})
			if len(diagnostics) != 0 {
				t.Fatal(diagnostics)
			}
			fastCode, fastSymbols, err := emit.LowerUnvalidatedWithSymbols(fastHIR)
			if err != nil {
				t.Fatal(err)
			}
			fullCode, fullSymbols, err := emit.LowerUnvalidatedWithSymbols(fullHIR)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fastCode, fullCode) || !reflect.DeepEqual(fastSymbols, fullSymbols) {
				t.Fatal("specialization fast path changed code or source symbols")
			}
		})
	}
}
