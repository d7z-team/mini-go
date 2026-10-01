package compiler

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestCompileWorkspaceRejectsImportedUnexportedStructCompositeField(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/lib",
		Files: []SourceFile{{
			Path: "lib.mgo",
			Text: `package lib
type Box struct {
	Value int64
	hidden int64
}
`,
		}},
	}, {
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `package main
import "example/lib"
func Main() { _ = lib.Box{hidden: 1} }
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	requireCompileDiagnostic(t, result.Diagnostics, "semantic.composite.unexported")
}

func TestCompileRejectsDuplicateConstantMapCompositeKeys(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name: "string",
			source: `
package main
func Main() { _ = map[string]int64{"go": 1, "go": 2} }
`,
		},
		{
			name: "target converted integer",
			source: `
package main
func Main() { _ = map[int64]int64{1: 1, int64(1): 2} }
`,
		},
		{
			name: "named string key",
			source: `
package main
type Key string
func Main() { _ = map[Key]int64{"go": 1, Key("go"): 2} }
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, "hirgen.composite.map.duplicate")
		})
	}
}

func TestCompileRejectsUncomparableMapKeyTypes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name: "type declaration slice key",
			source: `
package main
type Bad map[[]int64]int64
`,
		},
		{
			name: "type declaration alias slice key",
			source: `
package main
type Slice = []int64
type Bad map[Slice]int64
`,
		},
		{
			name: "struct field map key",
			source: `
package main
type Box struct {
	Lookup map[[]int64]int64
}
`,
		},
		{
			name: "function parameter map key",
			source: `
package main
func Use(map[[]int64]int64) {}
`,
		},
		{
			name: "local type declaration key",
			source: `
package main
func Main() {
	type Slice []int64
	type Bad map[Slice]int64
	_ = Bad{}
}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, "semantic.map.key.comparable")
		})
	}
}

func TestCompileAcceptsComparableMapKeyTypes(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main

type Key struct {
	Values [2]int64
}

type Lookup map[Key]int64

func Main() {
	type LocalKey struct {
		Name string
	}
	type LocalLookup map[LocalKey]int64
	_ = Lookup{}
	_ = LocalLookup{}
}
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
}

func TestCompileRejectsInvalidArraySliceCompositeKeys(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{
			name: "duplicate const slice key",
			source: `
package main
const Index = 2
func Main() { _ = []int64{Index: 1, 1 + 1: 2} }
`,
			code: "hirgen.composite.array.duplicate",
		},
		{
			name: "non integer slice key",
			source: `
package main
func Main() { _ = []int64{1.5: 1} }
`,
			code: "hirgen.composite.array.key",
		},
		{
			name: "negative array key",
			source: `
package main
func Main() { _ = [2]int64{-1: 1} }
`,
			code: "hirgen.composite.array.key",
		},
		{
			name: "array key out of range",
			source: `
package main
const Index = 2
func Main() { _ = [2]int64{Index: 1} }
`,
			code: "hirgen.composite.array.index",
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

func TestCompileInfersEllipsisArrayCompositeLength(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main

var Empty = [...]int64{}
var Values = [...]int64{1, 2, 3}
var Keyed = [...]int64{2: 5}
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
	want := map[string]string{
		"Empty":  "Array<0, Int64>",
		"Values": "Array<3, Int64>",
		"Keyed":  "Array<3, Int64>",
	}
	globalNames := packageGlobalNames(result.Symbols)
	for _, global := range result.Artifact.Globals {
		name := globalNames[global.ID]
		if typ, ok := want[name]; ok {
			if got := types.FormatWithTable(&result.Artifact.TypeTable, global.Type); got != typ {
				t.Fatalf("global %s type = %q, want %q", name, got, typ)
			}
			delete(want, name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing globals: %#v", want)
	}
}

func TestCompileRejectsArrayLengthEllipsisOutsideCompositeLiteral(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name: "type declaration",
			source: `
package main
type T [...]int64
`,
		},
		{
			name: "var declaration type",
			source: `
package main
var X [...]int64
`,
		},
		{
			name: "function parameter",
			source: `
package main
func F(x [...]int64) {}
`,
		},
		{
			name: "conversion",
			source: `
package main
func Main() { _ = [...]int64(nil) }
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, "semantic.type.array.infer")
		})
	}
}
