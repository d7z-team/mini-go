package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	sourceformat "github.com/d7z-team/mini-go/tooling/format"
)

func TestRPCGenerateDeterministicallyOverwritesBinding(t *testing.T) {
	directory := t.TempDir()
	for name, data := range map[string]string{
		"host.mgo":     "package host\n",
		"service.mrpc": "syntax = \"mrpc/v2\"; namespace example.host.v1; option go_package = \"example/host;hostbindings\"; service Clock { Now() returns (value int64 = 1); }\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outputPath := filepath.Join(directory, "bindings.go")
	mgoOutputPath := filepath.Join(directory, "bindings.mgo")
	args := []string{"generate", "-go-out", outputPath, "-mgo-out", mgoOutputPath, "-mgo-package", "host", "service.mrpc"}
	environment := testCommandEnvironment(t, directory)
	if err := runRPC(environment, args, io.Discard); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Contains(generated, []byte("package hostbindings")) || !bytes.Contains(generated, []byte("NewClockProvider")) || !bytes.Contains(generated, []byte("BindClockClient")) {
		t.Fatalf("generated binding = %q, %v", generated, err)
	}
	generatedMGO, err := os.ReadFile(mgoOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	formatted := sourceformat.Source("example.host.v1", mgoOutputPath, string(generatedMGO))
	if len(formatted.Diagnostics) != 0 || formatted.Text != string(generatedMGO) {
		t.Fatalf("generated MiniGo binding is not canonical: diagnostics=%v", formatted.Diagnostics)
	}
	if err := os.WriteFile(outputPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runRPC(environment, args, io.Discard); err != nil {
		t.Fatal(err)
	}
	regenerated, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(regenerated, generated) {
		t.Fatal("regenerated binding differs")
	}
}

func TestRPCGenerateRustFailurePreservesAllOutputs(t *testing.T) {
	if _, err := exec.LookPath("rustfmt"); err != nil {
		t.Skip("Rust formatting tool is not installed")
	}
	directory := t.TempDir()
	schema := `syntax = "mrpc/v2"; namespace example.tx.v1; option go_package = "example/tx;tx"; option rust_module = "crate::tx"; service Tx { Apply() returns (); }`
	if err := os.WriteFile(filepath.Join(directory, "service.mrpc"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	goOutput, rustOutput := filepath.Join(directory, "binding.go"), filepath.Join(directory, "binding.rs")
	for _, path := range []string{goOutput, rustOutput} {
		if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"generate", "-go-out", goOutput, "-rust-out", rustOutput, "-rust-module", "crate::mod", "service.mrpc"}
	if err := runRPC(testCommandEnvironment(t, directory), args, io.Discard); err == nil {
		t.Fatal("invalid Rust module accepted")
	}
	for _, path := range []string{goOutput, rustOutput} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Fatalf("partial output %s: %q, %v", path, data, err)
		}
	}
	args[6] = "crate::tx"
	if err := runRPC(testCommandEnvironment(t, directory), args, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rustOutput)
	if err != nil || !bytes.Contains(data, []byte("pub trait TxHandler")) {
		t.Fatalf("Rust output: %q, %v", data, err)
	}
	// Formatting failure occurs before committing even an otherwise valid Go target.
	t.Setenv("PATH", t.TempDir())
	if err := runRPC(testCommandEnvironment(t, directory), args, io.Discard); err == nil {
		t.Fatal("missing formatter accepted")
	}
	after, err := os.ReadFile(rustOutput)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("formatter failure changed output: %v", err)
	}
}

func TestRPCGenerateResolvesImportedContracts(t *testing.T) {
	directory := t.TempDir()
	files := map[string]string{
		"model.mrpc": `syntax = "mrpc/v2";
namespace example.model.v1;
option go_package = "example/model;model";
message Model { Name string = 1; }
`,
		"service.mrpc": `syntax = "mrpc/v2";
namespace example.service.v1;
option go_package = "example/service;service";
import model "model.mrpc";
service Service { Get(value model.Model = 1) returns (); }
`,
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(directory, "binding.go")
	if err := runRPC(testCommandEnvironment(t, directory), []string{"generate", "-go-out", output, "service.mrpc"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(output)
	if err != nil || !bytes.Contains(generated, []byte(`model "example/model"`)) || !bytes.Contains(generated, []byte(`model.EncodeModel(arg0)`)) {
		t.Fatalf("generated imported binding = %q, %v", generated, err)
	}
}

func TestRPCGenerateDoesNotPartiallyCommitOutputs(t *testing.T) {
	directory := t.TempDir()
	contract := `syntax = "mrpc/v2"; namespace example.tx.v1; option go_package = "example/tx;tx"; service Tx { Apply() returns (); }`
	if err := os.WriteFile(filepath.Join(directory, "service.mrpc"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "binding.out")
	if err := os.WriteFile(output, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runRPC(testCommandEnvironment(t, directory), []string{
		"generate", "-go-out", output, "-mgo-out", output, "-mgo-package", "tx", "service.mrpc",
	}, io.Discard)
	if err == nil {
		t.Fatal("duplicate transaction output was accepted")
	}
	data, readErr := os.ReadFile(output)
	if readErr != nil || string(data) != "original" {
		t.Fatalf("output after failed transaction = %q, %v", data, readErr)
	}
}

func TestRPCGenerateTypeScriptSharesAtomicTransaction(t *testing.T) {
	directory := t.TempDir()
	contract := `syntax = "mrpc/v2"; namespace example.web.v1; option go_package = "example/web;web"; option ts_module = "./binding.js"; message Request { value int64 = 1; } service Web { Echo(request Request = 1) returns (request Request = 1); }`
	if err := os.WriteFile(filepath.Join(directory, "service.mrpc"), []byte(contract), 0o644); err != nil {
		t.Fatal(err)
	}
	goOutput := filepath.Join(directory, "binding.go")
	typescriptOutput := filepath.Join(directory, "binding.ts")
	args := []string{"generate", "-go-out", goOutput, "-ts-out", typescriptOutput, "service.mrpc"}
	if err := runRPC(testCommandEnvironment(t, directory), args, io.Discard); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(typescriptOutput)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][]byte{[]byte("export interface Request"), []byte("createWebProvider"), []byte("class WebClient")} {
		if !bytes.Contains(generated, expected) {
			t.Fatalf("TypeScript output misses %q", expected)
		}
	}
	beforeGo, err := os.ReadFile(goOutput)
	if err != nil {
		t.Fatal(err)
	}
	beforeTypeScript := append([]byte(nil), generated...)
	failed := append([]string(nil), args...)
	failed = append(failed[:len(failed)-1], "-ts-prefix", "bad-", failed[len(failed)-1])
	if err := runRPC(testCommandEnvironment(t, directory), failed, io.Discard); err == nil {
		t.Fatal("invalid TypeScript target accepted")
	}
	afterGo, goErr := os.ReadFile(goOutput)
	afterTypeScript, typescriptErr := os.ReadFile(typescriptOutput)
	if goErr != nil || typescriptErr != nil || !bytes.Equal(beforeGo, afterGo) || !bytes.Equal(beforeTypeScript, afterTypeScript) {
		t.Fatalf("failed TypeScript generation partially committed outputs: %v, %v", goErr, typescriptErr)
	}
}
