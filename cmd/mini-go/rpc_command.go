package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	sourceformat "github.com/d7z-team/mini-go/tooling/format"
	"github.com/d7z-team/mini-go/tooling/mrpc"
	gogen "github.com/d7z-team/mini-go/tooling/mrpc/gen/go"
	mgogen "github.com/d7z-team/mini-go/tooling/mrpc/gen/mgo"
	rustgen "github.com/d7z-team/mini-go/tooling/mrpc/gen/rust"
	typescriptgen "github.com/d7z-team/mini-go/tooling/mrpc/gen/typescript"
)

func runRPC(environment commandEnvironment, args []string, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "generate" {
		return errors.New("usage: mini-go [-C directory] rpc generate [flags] file.mrpc [...]")
	}
	flags := flag.NewFlagSet("mini-go rpc generate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mgoOutput := flags.String("mgo-out", "", "generated Mini-Go source")
	goOutput := flags.String("go-out", "", "generated Go source")
	rustOutput := flags.String("rust-out", "", "generated Rust module source")
	rustModule := flags.String("rust-module", "", "Rust module path; defaults to rust_module option")
	rustRuntime := flags.String("rust-runtime", "", "Rust runtime crate path; defaults to rust_runtime option")
	rustPrefix := flags.String("rust-prefix", "", "prefix for generated Rust declarations")
	typescriptOutput := flags.String("ts-out", "", "generated TypeScript ESM source")
	typescriptRuntime := flags.String("ts-runtime", "", "TypeScript RPC SDK module; defaults to ts_runtime option")
	typescriptPrefix := flags.String("ts-prefix", "", "prefix for generated TypeScript declarations")
	mgoPackage := flags.String("mgo-package", "", "Mini-Go output package; defaults to mgo_package option")
	goPackage := flags.String("go-package", "", "Go output package; defaults to go_package option")
	goPrefix := flags.String("go-prefix", "", "prefix for generated Go declarations")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() == 0 || strings.TrimSpace(*mgoOutput) == "" && strings.TrimSpace(*goOutput) == "" && strings.TrimSpace(*rustOutput) == "" && strings.TrimSpace(*typescriptOutput) == "" {
		return errors.New("rpc generate requires contract files and at least one output")
	}
	sources := make([]mrpc.Source, 0, flags.NArg())
	for _, name := range flags.Args() {
		data, err := os.ReadFile(environment.path(name))
		if err != nil {
			return err
		}
		sources = append(sources, mrpc.Source{Path: filepath.ToSlash(name), Text: string(data)})
	}
	catalog, diagnostics, err := mrpc.LoadCatalog(sources, func(path string) (mrpc.Source, error) {
		data, readErr := os.ReadFile(environment.path(filepath.FromSlash(path)))
		return mrpc.Source{Path: path, Text: string(data)}, readErr
	})
	if err != nil {
		return err
	}
	if mrpc.HasErrors(diagnostics) {
		return fmt.Errorf("validate MRPC: %s", diagnostics[0].Message)
	}
	var outputs []atomicFile
	if output := strings.TrimSpace(*mgoOutput); output != "" {
		generated, err := mgogen.Generate(catalog, mgogen.Options{Package: *mgoPackage})
		if err != nil {
			return err
		}
		formatted := sourceformat.Source(catalog.Namespace, output, string(generated))
		if len(formatted.Diagnostics) != 0 {
			return fmt.Errorf("format generated Mini-Go MRPC: %s", formatted.Diagnostics[0].Message)
		}
		generated = []byte(formatted.Text)
		outputs = append(outputs, atomicFile{path: environment.path(output), data: generated, mode: 0o644})
	}
	if output := strings.TrimSpace(*goOutput); output != "" {
		generated, err := gogen.Generate(catalog, gogen.Options{Package: *goPackage, Prefix: *goPrefix})
		if err != nil {
			return err
		}
		outputs = append(outputs, atomicFile{path: environment.path(output), data: generated, mode: 0o644})
	}
	if output := strings.TrimSpace(*rustOutput); output != "" {
		generated, err := rustgen.Generate(catalog, rustgen.Options{Module: *rustModule, Runtime: *rustRuntime, Prefix: *rustPrefix})
		if err != nil {
			return err
		}
		command := exec.Command("rustfmt", "--edition", "2024", "--emit", "stdout", "--config", "skip_children=true")
		command.Stdin = bytes.NewReader(generated)
		var diagnostic bytes.Buffer
		command.Stderr = &diagnostic
		formatted, err := command.Output()
		if err != nil {
			return fmt.Errorf("format generated Rust MRPC: %w: %s", err, diagnostic.String())
		}
		outputs = append(outputs, atomicFile{path: environment.path(output), data: formatted, mode: 0o644})
	}
	if output := strings.TrimSpace(*typescriptOutput); output != "" {
		generated, err := typescriptgen.Generate(catalog, typescriptgen.Options{Runtime: *typescriptRuntime, Prefix: *typescriptPrefix})
		if err != nil {
			return err
		}
		outputs = append(outputs, atomicFile{path: environment.path(output), data: generated, mode: 0o644})
	}
	return writeFilesAtomically(outputs)
}
