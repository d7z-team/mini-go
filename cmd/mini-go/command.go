package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/d7z-team/mini-go/compiler/cache"
)

func runCLIContext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mini-go", flag.ContinueOnError)
	flags.SetOutput(stderr)
	directory := flags.String("C", ".", "change to directory before running the command")
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		return errors.New("usage: mini-go check|run|test|gateway|doc|rpc|fmt|lsp|dap|cache")
	}
	environment, err := newCommandEnvironment(*directory)
	if err != nil {
		return err
	}
	environment.ctx = ctx
	environment.stdin = os.Stdin
	switch args[0] {
	case "check":
		return runCheck(environment, args[1:], stderr)
	case "run":
		return runProgram(environment, args[1:], stdout, stderr)
	case "test":
		return runTestsContext(environment, args[1:], stdout, stderr)
	case "gateway":
		return runGateway(environment, args[1:], stdout, stderr)
	case "cache":
		return runCache(environment, args[1:], stdout)
	case "doc":
		return runDoc(environment, args[1:], stdout, stderr)
	case "fmt":
		return runFormat(environment, args[1:], os.Stdin, stdout, stderr)
	case "lsp":
		return runLSPContext(environment.context(), environment, os.Stdin, stdout)
	case "dap":
		return runDAPContext(environment.context(), environment, os.Stdin, stdout)
	case "rpc":
		return runRPC(environment, args[1:], stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runCache(environment commandEnvironment, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: mini-go cache clean|verify|inspect")
	}
	root, err := environment.cacheRoot(envCacheRoot, "cache")
	if err != nil {
		return err
	}
	backend := cache.NewDiskBackend(root)
	switch args[0] {
	case "clean":
		return backend.Clean()
	case "verify":
		return backend.Verify()
	case "inspect":
		stats, err := backend.Inspect()
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(stats)
	default:
		return fmt.Errorf("unknown cache command %q", args[0])
	}
}
