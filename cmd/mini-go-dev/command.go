package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/d7z-team/mini-go/tooling/runtimecheck"
)

func runDevCLI(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mini-go-dev unicode|compiler-binary|bytecode-json|compiler-core|compiler-identity|contract-spec|runtime-contract|runtime-vectors|runtime-state-vectors|runtime-stdlib-vectors|runtime-manifest|runtime-blocks|runtime-host-broker|rpc-fixtures|bootstrap|core-api|vscode-grammar")
	}
	switch args[0] {
	case "unicode":
		if len(args) != 1 {
			return errors.New("unicode accepts no arguments")
		}
		return generateUnicode()
	case "tools-schema":
		return runToolsSchema(args[1:], stderr)
	case "rpc-fixtures":
		return runRPCFixtures(args[1:], stderr)
	case "runtime-blocks":
		return runRuntimeBlocks(args[1:], stderr)
	case "runtime-host-broker":
		if len(args) != 1 {
			return errors.New("runtime-host-broker takes no arguments")
		}
		return runtimecheck.RunHostBroker(context.Background(), os.Stdin, stdout)
	case "compiler-binary":
		if len(args) != 1 {
			return errors.New("compiler-binary accepts no arguments")
		}
		return generateCompilerBinary()
	case "bytecode-json":
		if len(args) != 1 {
			return errors.New("bytecode-json accepts no arguments")
		}
		return generateBytecodeJSON()
	case "runtime-contract":
		return runRuntimeContract(args[1:], stderr)
	case "runtime-vectors", "runtime-stdlib-vectors":
		return runRuntimeVectorGeneration(args[0], args[1:], stderr)
	case "runtime-state-vectors":
		return runRuntimeStateVectors(args[1:], stderr)
	case "runtime-manifest":
		return runRuntimeManifest(args[1:], stderr)
	case "vscode-grammar":
		return runVSCodeGrammar(args[1:], stderr)
	case "compiler-identity":
		return runCompilerIdentity(args[1:], stderr)
	case "contract-spec":
		return runContractSpec(args[1:], stderr)
	case "compiler-core":
		return runCompilerCore(args[1:], stderr)
	case "bootstrap":
		return runBootstrap(args[1:], stdout, stderr)
	case "core-api":
		return runCoreAPI(args[1:], stdout)
	default:
		return fmt.Errorf("unknown development command %q", args[0])
	}
}
