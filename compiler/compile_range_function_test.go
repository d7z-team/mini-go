package compiler

import (
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileSourceValidatesRangeOverFunction(t *testing.T) {
	cases := []struct {
		name   string
		source string
		code   string
	}{{
		name: "target count",
		source: `package main

func Each(yield func(int) bool) {
	yield(1)
}

func Main() int {
	sum := 0
	for k, v := range Each {
		sum += k + v
	}
	return sum
}
`,
		code: "hirgen.range.func.targets",
	}, {
		name: "yield must return bool",
		source: `package main

func Each(yield func(int)) {
	yield(1)
}

func Main() int {
	sum := 0
	for v := range Each {
		sum += v
	}
	return sum
}
`,
		code: "hirgen.range.func.signature",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", tc.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			if result.OK() {
				t.Fatalf("expected diagnostic %s", tc.code)
			}
			found := false
			for _, diagnostic := range result.Diagnostics {
				if string(diagnostic.Code) == tc.code {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected diagnostic %s, got %#v", tc.code, result.Diagnostics)
			}
		})
	}
}

func TestCompileSourceLowersRangeOverFunctionDeferToOuterOwner(t *testing.T) {
	result, err := compileTestSource("example/main", "main.mgo", `package main

func Each(yield func(int) bool) {
	yield(1)
}

func Main() {
	for v := range Each {
		defer func(x int) {
			_ = x
		}(v)
	}
}
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
	found := false
	for _, symbol := range result.Symbols.Functions {
		if !strings.HasPrefix(symbol.Name, "range.yield.") {
			continue
		}
		fn, _, ok := artifactFunctionByName(result.Artifact, result.Symbols, symbol.Name)
		if !ok {
			continue
		}
		for _, inst := range functionOperations(t, fn) {
			if inst.Op != ir.OpDeferPush {
				continue
			}
			var payload ir.DeferPayload
			if err := ir.ReadInstructionPayload(inst.Payload, &payload); err != nil {
				t.Fatalf("defer_push payload decode failed: %v", err)
			}
			if payload.OwnerDepth != 2 {
				t.Fatalf("expected range yield defer owner depth 2, got %d", payload.OwnerDepth)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected range yield defer_push with owner depth")
	}
}
