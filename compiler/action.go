package compiler

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type EntryPoint struct {
	Name       string
	ModulePath string
	Function   string
}

type entrySelection struct {
	Name       string
	ModulePath string
	Function   string
}

type CheckResult struct {
	Target       target.Target
	Documents    map[string][]parser.Document
	ExportHashes map[string]string
	Order        []string
	GraphHash    string
	Diagnostics  []source.Diagnostic
	Stats        Stats
}

type PrepareResult struct {
	Checked      CheckResult
	Image        *ir.ExecutionImage
	Symbols      *ir.ProgramSymbols
	TestManifest []cache.TestEntry
}

func (r CheckResult) OK() bool {
	return !source.HasErrors(r.Diagnostics)
}

func Identity() string {
	return ir.CompilerIdentity
}

// Check parses and semantically analyzes a source graph without lowering it.
func Check(request Request) (CheckResult, error) {
	return checkWorkspace(request, []string{request.Root})
}

// CheckPackages parses and semantically analyzes the union of the requested
// package dependency graphs without lowering or emitting runtime artifacts.
func CheckPackages(request Request, roots []string) (CheckResult, error) {
	return checkWorkspace(request, roots)
}

// Prepare checks a source graph and prepares its explicit executable closure.
func Prepare(request Request) (PrepareResult, error) {
	return prepare(request, "prepare", nil)
}

func prepare(request Request, mode string, manifest []cache.TestEntry) (PrepareResult, error) {
	request.Context = requestContext(request.Context)
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, err
	}
	request.Limits = normalizeCompilerLimits(request.Limits)
	if err := validateOptimizationLevel(request.Optimization); err != nil {
		return PrepareResult{}, err
	}
	if request.Cache != nil && !request.CacheVerify {
		prepared, handled, err := lookupPrepared(request, mode)
		if err != nil || handled {
			return prepared, err
		}
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, err
	}
	built, err := compile(request, false)
	if err != nil {
		return PrepareResult{}, err
	}
	if err := request.Context.Err(); err != nil {
		return PrepareResult{}, err
	}
	return prepareBuiltResult(request, built, mode, manifest)
}

func prepareEntries(request Request, rootPackage string) ([]entrySelection, *source.Diagnostic) {
	entries := make([]entrySelection, 0, len(request.EntryPoints))
	for _, entry := range request.EntryPoints {
		modulePath := strings.TrimSpace(entry.ModulePath)
		if modulePath == "" {
			modulePath = strings.TrimSpace(request.Root)
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = ir.DefaultEntryName
		}
		entries = append(entries, entrySelection{Name: name, ModulePath: modulePath, Function: strings.TrimSpace(entry.Function)})
	}
	if len(entries) == 0 {
		if rootPackage == "main" {
			entries = append(entries, entrySelection{Name: ir.DefaultEntryName, ModulePath: request.Root, Function: "main"})
		} else {
			diagnostic := source.Diagnostic{
				Code: "compiler.entry.required", Severity: source.SeverityError,
				Message: "non-main package requires an explicit entry point",
			}
			return nil, &diagnostic
		}
	}
	return entries, nil
}
