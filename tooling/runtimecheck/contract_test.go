package runtimecheck

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRuntimeContractTypeEnums(t *testing.T) {
	intrinsics, err := os.ReadFile("../../runtime/bytecode/intrinsic.go")
	if err != nil {
		t.Fatal(err)
	}
	const model = `package types
const ()
type Kind uint8
const (
	Invalid Kind = iota
	Void
	Any
)
type PrimitiveKind uint8
const (
	PrimitiveInvalid PrimitiveKind = iota
	PrimitiveBool
)
type ChannelDir uint8
const (
	ChannelInvalid ChannelDir = iota
	ChannelBoth
)
`
	for _, generator := range []struct {
		name string
		run  func(fs.FS) ([]byte, error)
	}{
		{"Rust", GenerateRustContract},
		{"TypeScript", GenerateTypeScriptContract},
	} {
		t.Run(generator.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name, source, diagnostic string
			}{
				{"iota ordinals", model, ""},
				{"missing group", strings.ReplaceAll(model, "ChannelDir", "Unrelated"), "missing compiler type enums"},
				{"explicit base", strings.ReplaceAll(model, "Invalid Kind = iota", "Invalid Kind = 1"), "expected iota: Kind"},
				{"explicit later value", strings.ReplaceAll(model, "\n\tVoid\n", "\n\tVoid = 8\n"), "unsupported enum declaration Kind"},
				{"multiple names", strings.ReplaceAll(model, "\n\tVoid\n", "\n\tVoid, Extra\n"), "unsupported enum declaration Kind"},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					sources := fstest.MapFS{
						"runtime/bytecode/intrinsic.go": {Data: intrinsics},
						"compiler/types/model.go":       {Data: []byte(scenario.source)},
					}
					data, err := generator.run(sources)
					if scenario.diagnostic != "" {
						if err == nil || !strings.Contains(err.Error(), scenario.diagnostic) {
							t.Fatalf("diagnostic = %v, want %q", err, scenario.diagnostic)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					for name, ordinal := range map[string]int{
						"Invalid": 0, "Void": 1, "Any": 2,
						"PrimitiveInvalid": 0, "PrimitiveBool": 1,
						"ChannelInvalid": 0, "ChannelBoth": 1,
					} {
						declaration := fmt.Sprintf("pub const %s: u8 = %d;", name, ordinal)
						if generator.name == "TypeScript" {
							declaration = fmt.Sprintf("  %s: %d,", name, ordinal)
						}
						if !strings.Contains(string(data), declaration) {
							t.Errorf("missing enum declaration %q", declaration)
						}
					}
				})
			}
		})
	}
}
