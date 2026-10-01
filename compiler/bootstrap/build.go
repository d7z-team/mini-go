// Package bootstrap builds and validates the portable Mini-Go compiler program.
package bootstrap

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/stdlib"
)

const compilerModulePath = "github.com/d7z-team/mini-go/compiler"

type BuildOptions struct {
	Filesystem fs.FS
	Root       string
	Cache      cache.Backend
	TraceCache func(cache.Event)
}

type imageSpec struct {
	Root    string
	Entries []compiler.EntryPoint
}

var compilerImage = imageSpec{
	Root: compilerModulePath + "/bootstrap/compilerentry",
	Entries: []compiler.EntryPoint{
		{Name: "default", ModulePath: compilerModulePath + "/bootstrap/compilerentry", Function: "Compile"},
		{Name: "tools", ModulePath: compilerModulePath + "/bootstrap/compilerentry", Function: "Tools"},
	},
}

func newCompilerSession(options BuildOptions) (workspace.SourceSet, *compiler.Compiler, error) {
	if options.Filesystem == nil {
		return nil, nil, errors.New("nil bootstrap source filesystem")
	}
	root := strings.TrimSpace(options.Root)
	if root == "" {
		root = "."
	}
	if root != "." && !fs.ValidPath(root) {
		return nil, nil, fmt.Errorf("invalid bootstrap source root %q", root)
	}
	project, err := fs.Sub(options.Filesystem, root)
	if err != nil {
		return nil, nil, err
	}
	compilerSources, err := compilerSourceTree(project, "compiler", compilerModulePath)
	if err != nil {
		return nil, nil, err
	}
	bytecodeSources, err := compilerSourceTree(project, "runtime/bytecode", "github.com/d7z-team/mini-go/runtime/bytecode")
	if err != nil {
		return nil, nil, err
	}
	library := stdlib.Open()
	standardSources, err := workspace.StandardLibrary(library)
	if err != nil {
		return nil, nil, err
	}
	sources, err := workspace.MergeSourceSets(compilerSources, bytecodeSources, standardSources)
	if err != nil {
		return nil, nil, err
	}
	var compileCache cache.Cache
	if options.Cache != nil {
		compileCache = cache.New(options.Cache)
	}
	session, err := compiler.New(compiler.Options{
		Sources: sources, Cache: compileCache, TraceCache: options.TraceCache,
		Optimization: compiler.OptimizationFull,
	})
	if err != nil {
		return nil, nil, err
	}
	return sources, session, nil
}
