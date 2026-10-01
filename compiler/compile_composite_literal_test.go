package compiler

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileAssignsFieldThroughSliceIndex(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main
type item struct { Value int }
func Main() int {
	items := make([]item, 1)
	items[0].Value = 42
	return items[0].Value
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Fatalf("compile diagnostics: %#v", result.Diagnostics)
	}
}

func TestCompileAssignsFieldThroughMapPointer(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main
type item struct { Value int }
func Main() int {
	value := &item{}
	items := map[string]*item{"key": value}
	items["key"].Value = 42
	return value.Value
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Fatalf("compile diagnostics: %#v", result.Diagnostics)
	}
}

func TestCompileRejectsFieldAssignmentThroughMapValue(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main
type item struct { Value int }
func Main() {
	items := map[string]item{"key": {}}
	items["key"].Value = 42
}
`)
	if err != nil {
		t.Fatal(err)
	}
	requireCompileDiagnostic(t, result.Diagnostics, "semantic.assign.target")
}

func TestCompileSlicesAddressableArrayThroughPointer(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main
func Main() int {
	values := [3]byte{1, 2, 3}
	view := values[1:]
	view[0] = 9
	return int(values[1])
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Fatalf("compile diagnostics: %#v", result.Diagnostics)
	}
	if function, _, ok := artifactFunctionByName(result.Artifact, result.Symbols, "Main"); ok {
		for index, instruction := range functionOperations(t, function) {
			if instruction.Op != ir.OpSlice {
				continue
			}
			for previous := index - 1; previous >= 0; previous-- {
				if functionOperations(t, function)[previous].Op == ir.OpAddressOf {
					return
				}
			}
			t.Fatalf("array slice does not consume an address: %#v", functionOperations(t, function))
		}
	}
	t.Fatal("Main slice instruction not found")
}

func TestCompileRejectsSlicingUnaddressableArray(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main
func Main() { _ = [2]int{1, 2}[:] }
`)
	if err != nil {
		t.Fatal(err)
	}
	requireCompileDiagnostic(t, result.Diagnostics, "semantic.slice.array_addressable")
}

func TestCompileRejectsInvalidStructCompositeLiterals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{
			name: "unkeyed too few fields",
			source: `
package main
type Point struct { X int64; Y int64 }
func Main() { _ = Point{1} }
`,
			code: "hirgen.composite.struct.field_count",
		},
		{
			name: "unkeyed too many fields",
			source: `
package main
type Point struct { X int64 }
func Main() { _ = Point{1, 2} }
`,
			code: "hirgen.composite.struct.field_count",
		},
		{
			name: "mixed keyed and unkeyed",
			source: `
package main
type Point struct { X int64; Y int64 }
func Main() { _ = Point{1, Y: 2} }
`,
			code: "hirgen.composite.struct.mixed",
		},
		{
			name: "duplicate field",
			source: `
package main
type Point struct { X int64 }
func Main() { _ = Point{X: 1, X: 2} }
`,
			code: "hirgen.composite.struct.duplicate",
		},
		{
			name: "unknown field",
			source: `
package main
type Point struct { X int64 }
func Main() { _ = Point{Y: 1} }
`,
			code: "hirgen.composite.struct.unknown",
		},
		{
			name: "non identifier key",
			source: `
package main
type Point struct { X int64 }
func Main() { _ = Point{"X": 1} }
`,
			code: "hirgen.composite.struct.field",
		},
		{
			name: "promoted field key",
			source: `
package main
type Inner struct { Value int64 }
type Outer struct { Inner }
func Main() { _ = Outer{Value: 1} }
`,
			code: "hirgen.composite.struct.unknown",
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

func TestCompileRejectsInvalidCompositeLiteralBroadMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		code   string
	}{
		{
			name: "map literal missing key",
			source: `
package main
func Main() { _ = map[string]int64{1} }
`,
			code: "hirgen.composite.map.key.missing",
		},
		{
			name: "array length non integer constant",
			source: `
package main
const N = 1.5
func Main() { _ = [N]int64{} }
`,
			code: "semantic.array.length.integer",
		},
		{
			name: "array length non constant",
			source: `
package main
func Main() {
	n := 2
	_ = [n]int64{}
}
`,
			code: "semantic.array.length.constant",
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

func TestCompileSourceAllowsRecursiveElidedCompositeLiterals(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `
package main

type Point struct {
	X int64
	Y int64
}

type Pair [2]Point

type Box struct {
	Head Point
	Points []Point
	Lookup map[Point]Point
	Ptr *Point
}

func Main() {
	_ = []Point{{X: 1}, {2, 3}}
	_ = map[Point]Point{{X: 1}: {Y: 2}}
	_ = Box{
		Head: {X: 3},
		Points: {{4, 5}},
		Lookup: map[Point]Point{{}: {X: 6}},
		Ptr: {X: 7},
	}
	_ = []Pair{{{1, 2}, {3, 4}}}
}
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
}

func TestCompileSourceRejectsElidedCompositeOutsideCompositeLiteral(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{
			name: "package var",
			source: `
package main
type Point struct { X int64 }
var P Point = {X: 1}
`,
		},
		{
			name: "local assignment",
			source: `
package main
type Point struct { X int64 }
func Main() {
	var p Point
	p = {X: 1}
}
`,
		},
		{
			name: "return",
			source: `
package main
type Point struct { X int64 }
func Make() Point {
	return {X: 1}
}
`,
		},
		{
			name: "call argument",
			source: `
package main
type Point struct { X int64 }
func Use(Point) {}
func Main() {
	Use({X: 1})
}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, "hirgen.composite.elided_context")
		})
	}
}
