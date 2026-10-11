package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/d7z-team/mini-go/tooling/runtimecheck"
)

func runRuntimeBlocks(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime-blocks", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("out", "", "output directory for precompiled blocks")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" || flags.NArg() == 0 {
		return errors.New("runtime-blocks requires -out and .mgo source files")
	}
	for _, path := range flags.Args() {
		if filepath.Ext(path) != ".mgo" {
			return fmt.Errorf("block source must use .mgo: %s", path)
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(filepath.Base(path), ".mgo")
		image, err := runtimecheck.CompileBlock("examples/"+name, string(source))
		if err != nil {
			return err
		}
		if err := writeGeneratedFile(filepath.Join(*output, name+".json"), image); err != nil {
			return err
		}
	}
	return nil
}

func runRuntimeManifest(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime-manifest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "testdata/runtime", "shared runtime fixture directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	data, err := runtimecheck.GenerateManifest(os.DirFS(*root))
	if err != nil {
		return err
	}
	return writeGeneratedFile(filepath.Join(*root, "manifest.json"), data)
}

func runRuntimeStateVectors(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime-state-vectors", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("out", "", "generated owner state observations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" || flags.NArg() == 0 {
		return errors.New("runtime-state-vectors requires -out and artifact files")
	}
	inputs := make(map[string][]byte)
	for _, path := range flags.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		if _, found := inputs[name]; found {
			return fmt.Errorf("duplicate state case %q", name)
		}
		inputs[name] = data
	}
	data, err := runtimecheck.GenerateStateVectors(context.Background(), inputs)
	if err != nil {
		return err
	}
	return writeGeneratedFile(*output, data)
}

func runRuntimeContract(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("runtime-contract", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("out", "", "generated Rust contract declarations")
	typeScriptOutput := flags.String("typescript-out", "", "generated TypeScript contract declarations")
	vectors := flags.String("vectors", "", "canonical wire verification inputs")
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" {
		return errors.New("runtime-contract requires -out")
	}
	data, err := runtimecheck.GenerateRustContract(os.DirFS(*root))
	if err != nil {
		return err
	}
	oracle, err := runtimecheck.GenerateWireVectors()
	if err != nil {
		return err
	}
	if err := writeGeneratedFile(*output, data); err != nil {
		return err
	}
	if *typeScriptOutput != "" {
		data, err = runtimecheck.GenerateTypeScriptContract(os.DirFS(*root))
		if err != nil {
			return err
		}
		if err := writeGeneratedFile(*typeScriptOutput, data); err != nil {
			return err
		}
	}
	if *vectors != "" {
		return writeGeneratedFile(*vectors, oracle)
	}
	return nil
}

func runRuntimeVectorGeneration(command string, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("out", "", "generated gzip runtime observations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" {
		return fmt.Errorf("%s requires -out", command)
	}
	var data []byte
	var err error
	switch command {
	case "runtime-vectors":
		data, err = runtimecheck.GenerateExecutionVectors(os.DirFS("testdata/runtime/source"))
	case "runtime-stdlib-vectors":
		data, err = runtimecheck.GenerateStdlibVectors()
	default:
		return fmt.Errorf("unknown runtime vector command %q", command)
	}
	if err != nil {
		return err
	}
	return writeGeneratedGzip(*output, data)
}
