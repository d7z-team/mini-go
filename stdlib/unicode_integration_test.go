package stdlib_test

import (
	"context"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
)

func TestUnicodeTablesAreInstanceOwnedAndRetainedAcrossPatch(t *testing.T) {
	const root = "minigo.test/unicode-state"
	const source = `package main
import "unicode"
func Read() int { return int(unicode.Letter.R16[0].Lo) + VERSION }
func Write() {
 unicode.Categories["L"].R16[0].Lo = 123
 if unicode.L != unicode.Letter { panic("letter alias changed") }
}
`
	entries := []compiler.EntryPoint{
		{Name: "read", ModulePath: root, Function: "Read"},
		{Name: "write", ModulePath: root, Function: "Write"},
	}
	before := prepareStdlibProgram(t, root, strings.ReplaceAll(source, "VERSION", "0"), entries)
	after := prepareStdlibProgram(t, root, strings.ReplaceAll(source, "VERSION", "1"), entries)
	ctx := context.Background()
	first, err := before.Instantiate(ctx, minigoruntime.InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := before.Instantiate(ctx, minigoruntime.InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := first.Call(ctx, "write"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		instance *minigoruntime.Instance
		want     int64
	}{{first, 123}, {second, 65}} {
		result, err := test.instance.Call(ctx, "read")
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := result.Values[0].Int64(); !ok || value != test.want {
			t.Fatalf("table ownership: got %v, want %d", result.Values, test.want)
		}
	}
	plan, err := first.PreparePatch(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	result, err := first.Call(ctx, "read")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 124 {
		t.Fatalf("patch reset mutable Unicode table: %v", result.Values)
	}
}
