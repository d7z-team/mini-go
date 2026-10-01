package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompilerIdentityCheckPreservesOutputAndDetectsSourceChanges(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, dir := range []string{"compiler", "runtime/bytecode"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"go.mod": "module example\n", "compiler/compiler.go": "package compiler\n",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join("runtime", "bytecode", "identity.go")
	if err := runCompilerIdentity([]string{"-out", output}, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(123456789, 0)
	if err := os.Chtimes(output, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"package compiler\n", "package compiler\nconst Changed = true\n"} {
		if err := os.WriteFile("compiler/compiler.go", []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		err := runCompilerIdentity([]string{"-out", output, "-check"}, io.Discard)
		if strings.Contains(source, "Changed") {
			if err == nil || !strings.Contains(err.Error(), "run make generate") {
				t.Fatalf("stale identity diagnostic = %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(output)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) || !info.ModTime().Equal(stamp) {
			t.Fatal("identity check modified the generated output")
		}
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	if err := runCompilerIdentity([]string{"-out", output, "-check"}, io.Discard); err == nil || !strings.Contains(err.Error(), "missing compiler identity") {
		t.Fatalf("missing identity diagnostic = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("missing output changed during check: %v", err)
	}
}
