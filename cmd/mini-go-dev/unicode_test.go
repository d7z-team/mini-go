package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/token"
)

func TestFrozenUnicodeNames(t *testing.T) {
	_, tables, err := unicodeTables()
	if err != nil {
		t.Fatal(err)
	}
	const maxRune = 0x10ffff
	expected := make([]byte, maxRune+1)
	for i, name := range []string{"_L", "_Nd", "_Lu"} {
		if len(tables[name]) == 0 {
			t.Fatalf("missing %s", name)
		}
		for _, row := range tables[name] {
			for r := row[0]; r <= row[1]; r += row[2] {
				expected[r] |= 1 << i
			}
		}
	}
	for r := rune(0); r <= maxRune; r++ {
		start := r == '_' || expected[r]&1 != 0
		part := start || expected[r]&2 != 0
		exported := expected[r]&4 != 0
		if token.IsIdentifierStart(r) != start || token.IsIdentifierPart(r) != part || token.IsExportedName(string(r)) != exported {
			t.Fatalf("Unicode name classification mismatch at U+%04X", r)
		}
	}
}

// Compile both generated and frozen Go data with the same query implementation.
// Comparing every variable also covers private tables, nil slices and aliases.
func TestPackedUnicodeMatchesFrozenTables(t *testing.T) {
	source, _, err := unicodeTables()
	if err != nil {
		t.Fatal(err)
	}
	packed, resource, err := packUnicodeTables(source)
	if err != nil {
		t.Fatal(err)
	}
	again, blob, err := packUnicodeTables(source)
	if err != nil || !bytes.Equal(packed, again) || !bytes.Equal(resource, blob) {
		t.Fatal("non-deterministic Unicode generation", err)
	}
	dir := t.TempDir()
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module unicodecheck\n\ngo 1.26\n"))
	files, err := filepath.Glob("../../stdlib/src/unicode/*.mgo")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(gotoken.NewFileSet(), "tables.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot strings.Builder
	snapshot.WriteString("package unicode\nfunc Snapshot() []any { return []any{")
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != gotoken.VAR {
			continue
		}
		for _, spec := range group.Specs {
			for _, name := range spec.(*ast.ValueSpec).Names {
				snapshot.WriteString(name.Name + ",")
			}
		}
	}
	snapshot.WriteString("} }\n")
	for _, name := range []string{"original", "packed"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			base := filepath.Base(path)
			if base == "tables.mgo" || strings.HasSuffix(base, "_test.mgo") {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			write(name+"/"+strings.TrimSuffix(base, ".mgo")+".go", data)
		}
		data := source
		if name == "packed" {
			data = packed
			write(name+"/ranges.bin", resource)
		}
		write(name+"/tables.go", data)
		write(name+"/snapshot.go", []byte(snapshot.String()))
	}
	write("main_test.go", []byte(`package unicodecheck
import (
 "encoding/json"
 "bytes"
 "reflect"
 "testing"
 a "unicodecheck/original"
 b "unicodecheck/packed"
)
func TestTables(t *testing.T) {
 original, packed := a.Snapshot(), b.Snapshot()
 x,e:=json.Marshal(original); if e!=nil {t.Fatal(e)}
 y,e:=json.Marshal(packed); if e!=nil {t.Fatal(e)}
 if !bytes.Equal(x,y) {t.Fatal("table values differ")}
 // Pointer equality between declarations must be preserved, not just values.
 for i,x:=range original { for j,y:=range original {
  vx,vy:=reflect.ValueOf(x),reflect.ValueOf(y)
  if vx.Kind()==reflect.Pointer && vy.Kind()==reflect.Pointer {
   if (vx.Pointer()==vy.Pointer()) != (reflect.ValueOf(packed[i]).Pointer()==reflect.ValueOf(packed[j]).Pointer()) { t.Fatal("alias mismatch",i,j) }
  }
 } }
 for r:=rune(0); r<=a.MaxRune; r++ {
  if a.IsLetter(r)!=b.IsLetter(r) || a.IsDigit(r)!=b.IsDigit(r) || a.IsUpper(r)!=b.IsUpper(r) || a.IsLower(r)!=b.IsLower(r) || a.ToUpper(r)!=b.ToUpper(r) || a.ToLower(r)!=b.ToLower(r) || a.ToTitle(r)!=b.ToTitle(r) || a.SimpleFold(r)!=b.SimpleFold(r) {t.Fatalf("codepoint U+%04X",r)}
 }
}`))
	command := exec.Command("go", "test", "./...")
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Unicode behavior: %v\n%s", err, output)
	}
}
