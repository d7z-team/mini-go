package compiler

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileLocalValueShadowsImportAliasInSelector(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{
		{
			ModulePath: "example/source",
			Files: []SourceFile{{Path: "source.mgo", Text: `
package source
const Exported = 1
`}},
		},
		{
			ModulePath: "example/main",
			Files: []SourceFile{{Path: "main.mgo", Text: `
package main
import "example/source"
type Package struct { Occurrences []int }
func Index(source Package) func() int {
	return func() int {
		source.Occurrences = nil
		return len(source.Occurrences)
	}
}
func Main() int { return Index(Package{})() + source.Exported }
`}},
		},
	})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
	artifact, ok := result.Artifact("example/main")
	if !ok {
		t.Fatal("expected main artifact")
	}
	for _, requirement := range artifact.Requirements {
		if requirement.ModulePath != "example/source" {
			continue
		}
		for _, export := range requirement.Exports {
			if export == "Occurrences" {
				t.Fatalf("shadowed import alias produced module requirement %q", export)
			}
		}
	}
}

func TestCompileSourceRejectsAmbiguousPromotedSelectors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{
			name: "promoted field read",
			source: `
package main
type Left struct { X int64 }
type Right struct { X int64 }
type Both struct { Left; Right }
func Main() int64 {
	var value Both
	return value.X
}
`,
			code: "hirgen.selector.ambiguous",
		},
		{
			name: "promoted field address",
			source: `
package main
type Left struct { X int64 }
type Right struct { X int64 }
type Both struct { Left; Right }
func Main() *int64 {
	var value Both
	return &value.X
}
`,
			code: "hirgen.selector.ambiguous",
		},
		{
			name: "promoted field assignment",
			source: `
package main
type Left struct { X int64 }
type Right struct { X int64 }
type Both struct { Left; Right }
func Main() {
	var value Both
	value.X = 1
}
`,
			code: "hirgen.selector.ambiguous",
		},
		{
			name: "promoted method call",
			source: `
package main
type Left struct{}
func (l Left) M() int64 { return 1 }
type Right struct{}
func (r Right) M() int64 { return 2 }
type Both struct { Left; Right }
func Main() int64 {
	var value Both
	return value.M()
}
`,
			code: "hirgen.method.ambiguous",
		},
		{
			name: "promoted method value",
			source: `
package main
type Left struct{}
func (l Left) M() int64 { return 1 }
type Right struct{}
func (r Right) M() int64 { return 2 }
type Both struct { Left; Right }
func Main() int64 {
	var value Both
	fn := value.M
	return fn()
}
`,
			code: "hirgen.selector.ambiguous",
		},
		{
			name: "promoted field and method",
			source: `
package main
type Left struct { M func() int64 }
type Right struct{}
func (r Right) M() int64 { return 2 }
type Both struct { Left; Right }
func Main() int64 {
	var value Both
	return value.M()
}
`,
			code: "hirgen.method.ambiguous",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, tc.code)
		})
	}
}

func TestCompileSourceAllowsDirectSelectorToShadowPromotedAmbiguity(t *testing.T) {
	for _, source := range []string{
		`
package main
type Left struct { X int64 }
type Right struct { X int64 }
type Both struct { Left; Right; X int64 }
func Main() int64 {
	return Both{X: 42}.X
}
`,
		`
package main
type Left struct{}
func (l Left) M() int64 { return 1 }
type Right struct{}
func (r Right) M() int64 { return 2 }
type Both struct { Left; Right }
func (b Both) M() int64 { return 42 }
func Main() int64 {
	var value Both
	return value.M()
}
`,
	} {
		result, err := compileTestSource("example/main", "main.mgo", source)
		if err != nil {
			t.Fatalf("compileTestSource failed: %v", err)
		}
		if !result.OK() {
			t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
		}
	}
}

func TestCompilePromotedFieldAddressUsesCompletePath(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main

type Lock struct{}
func (lock *Lock) Lock() {}

type common struct { mu Lock }
type Template struct { *common }

func Main(template *Template) { template.mu.Lock() }
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
	artifact := result.Artifact
	if function, _, ok := artifactFunctionByName(artifact, result.Symbols, "Main"); ok {
		for _, instruction := range functionOperations(t, function) {
			if instruction.Op != ir.OpAddressOf {
				continue
			}
			var payload ir.AddressPayload
			if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Path) == 2 && payload.Path[0].Field == "common" && payload.Path[1].Field == "mu" {
				return
			}
		}
	}
	t.Fatal("promoted field address did not contain common.mu")
}

func TestCompileSourceRejectsAmbiguousPromotedMethodInterfaceSatisfaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{
			name: "value method set conflict",
			source: `
package main
type Contract interface { M() int64 }
type Left struct{}
func (l Left) M() int64 { return 1 }
type Right struct{}
func (r Right) M() int64 { return 2 }
type Both struct { Left; Right }
func Main() int64 {
	var value Contract = Both{}
	return value.M()
}
`,
			code: "semantic.assign.type",
		},
		{
			name: "pointer method set conflict",
			source: `
package main
type Contract interface { M() int64 }
type Left struct{}
func (l *Left) M() int64 { return 1 }
type Right struct{}
func (r *Right) M() int64 { return 2 }
type Both struct { Left; Right }
func Main() int64 {
	var value Contract = &Both{}
	return value.M()
}
`,
			code: "semantic.assign.type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, tc.code)
		})
	}
}

func TestCompileSourceRejectsNonAddressablePointerReceiverMethodValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{
			name: "map element",
			source: `
package main
type Box struct { Value int64 }
func (b *Box) Inc() {}
func Main() {
	var boxes map[string]Box
	_ = boxes["x"].Inc
}
`,
			code: "hirgen.addr.index.map",
		},
		{
			name: "function result",
			source: `
package main
type Box struct { Value int64 }
func (b *Box) Inc() {}
func Make() Box { return Box{} }
func Main() {
	_ = Make().Inc
}
`,
			code: "hirgen.addr.target",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, tc.code)
		})
	}
}

func TestCompileSourceAllowsValidExpressionStatements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name: "function call",
			source: `
package main
func value() int64 { return 1 }
func Main() { value() }
`,
		},
		{
			name: "shadowed builtin call",
			source: `
package main
func len(value string) {}
func Main() { len("value") }
`,
		},
		{
			name: "receive operation",
			source: `
package main
func Main() {
	ch := make(chan int64, 1)
	ch <- 1
	<-ch
}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			if !result.OK() {
				t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
			}
		})
	}
}
