package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func runCLI(args []string, stdout, stderr io.Writer) error {
	return runCLIContext(context.Background(), args, stdout, stderr)
}

func writeCommandFile(t *testing.T, root, name, text string) {
	t.Helper()
	filename := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testCommandEnvironment(t *testing.T, directory string) commandEnvironment {
	t.Helper()
	environment, err := newCommandEnvironment(directory)
	if err != nil {
		t.Fatal(err)
	}
	environment.tempDir = t.TempDir()
	return environment
}
