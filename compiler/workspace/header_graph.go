package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
)

type HeaderGraph struct {
	Root        string
	Roots       []string
	Target      target.Target
	Packages    map[string]PackageHeader
	Order       []string
	Hash        string
	Diagnostics []source.Diagnostic
}

func LoadHeaders(root string, sources SourceSet, buildTarget target.Target) (HeaderGraph, error) {
	return LoadHeadersWithLimits(root, sources, buildTarget, Limits{})
}

func LoadHeadersWithLimits(root string, sources SourceSet, buildTarget target.Target, limits Limits) (HeaderGraph, error) {
	return LoadHeadersForRootsWithLimits([]string{root}, sources, buildTarget, limits)
}

// LoadHeadersForRoots builds one deterministic dependency graph for roots.
func LoadHeadersForRoots(roots []string, sources SourceSet, buildTarget target.Target) (HeaderGraph, error) {
	return LoadHeadersForRootsWithLimits(roots, sources, buildTarget, Limits{})
}

func LoadHeadersForRootsWithLimits(roots []string, sources SourceSet, buildTarget target.Target, limits Limits) (HeaderGraph, error) {
	loader, err := NewLoader(sources, buildTarget, limits)
	if err != nil {
		return HeaderGraph{}, err
	}
	return loader.LoadHeadersForRoots(roots)
}

func (l *Loader) loadHeadersForRoots(roots []string) (HeaderGraph, error) {
	limits := l.limits
	if len(roots) == 0 {
		return HeaderGraph{}, errors.New("workspace requires at least one root module")
	}
	normalizedRoots := make([]string, 0, len(roots))
	seenRoots := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		normalized, err := normalizeModulePath(root)
		if err != nil {
			return HeaderGraph{}, err
		}
		if _, exists := seenRoots[normalized]; exists {
			continue
		}
		seenRoots[normalized] = struct{}{}
		normalizedRoots = append(normalizedRoots, normalized)
	}
	sort.Strings(normalizedRoots)
	if l.sources == nil {
		return HeaderGraph{}, errors.New("nil source set")
	}
	normalizedTarget := l.target
	graph := HeaderGraph{Roots: normalizedRoots, Target: normalizedTarget, Packages: map[string]PackageHeader{}}
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	if len(normalizedRoots) == 1 {
		graph.Root = normalizedRoots[0]
	}
	state := map[string]uint8{}
	discovered := map[string]struct{}{}
	fileCount, sourceBytes := 0, 0
	var stack []string
	var visit func(string, source.Span) bool
	visit = func(modulePath string, importedAt source.Span) bool {
		switch state[modulePath] {
		case 2:
			return true
		case 1:
			start := 0
			for i, item := range stack {
				if item == modulePath {
					start = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), modulePath)
			diagnostic := workspaceDiagnostic("compiler.workspace.module.cycle", "module dependency cycle: "+strings.Join(cycle, " -> "))
			diagnostic.Primary = importedAt
			if len(stack) != 0 {
				diagnostic.ModulePath = stack[len(stack)-1]
			}
			collector.Add(diagnostic)
			return false
		}
		if _, exists := discovered[modulePath]; !exists {
			discovered[modulePath] = struct{}{}
			if len(discovered) > limits.MaxPackages {
				collector.Add(workspaceDiagnostic("compiler.limit.packages", "source graph exceeds package limit"))
				return false
			}
		}
		pkg, header, diagnostics, ok, loadErr := l.loadPackageHeader(modulePath)
		if loadErr != nil {
			diagnostic := workspaceDiagnostic("compiler.workspace.source.read", fmt.Sprintf("read module %q: %v", modulePath, loadErr))
			diagnostic.Primary = importedAt
			if len(stack) != 0 {
				diagnostic.ModulePath = stack[len(stack)-1]
			}
			collector.Add(diagnostic)
			return false
		}
		if !ok {
			diagnostic := workspaceDiagnostic("compiler.workspace.module.missing", fmt.Sprintf("missing source module %q", modulePath))
			diagnostic.Primary = importedAt
			if len(stack) != 0 {
				diagnostic.ModulePath = stack[len(stack)-1]
			}
			collector.Add(diagnostic)
			return false
		}
		for _, files := range [][]source.File{pkg.Files, pkg.TestFiles} {
			for _, file := range files {
				if fileCount == limits.MaxFiles {
					collector.Add(workspaceDiagnostic("compiler.limit.files", "source graph exceeds file limit"))
					return false
				}
				fileCount++
				if len(file.Text) > limits.MaxSourceBytes {
					diagnostic := workspaceDiagnostic("scanner.source.limit", "source file exceeds scanner byte limit")
					diagnostic.Primary, _ = file.Span(0, len(file.Text))
					collector.Add(diagnostic)
					return false
				}
				if len(file.Text) > limits.MaxTotalSourceBytes-sourceBytes {
					collector.Add(workspaceDiagnostic("compiler.limit.source_bytes", "source graph exceeds total byte limit"))
					return false
				}
				sourceBytes += len(file.Text)
			}
		}
		for i := range diagnostics {
			diagnostics[i].ModulePath = modulePath
		}
		collector.AddAll(diagnostics...)
		state[modulePath] = 1
		stack = append(stack, modulePath)
		valid := !source.HasErrors(diagnostics)
		for _, dependency := range header.Imports {
			if dependency == modulePath {
				collector.Add(workspaceDiagnostic("compiler.workspace.module.self_import", fmt.Sprintf("module %q imports itself", modulePath)))
				valid = false
				continue
			}
			if !visit(dependency, header.importSpans[dependency]) {
				valid = false
			}
		}
		stack = stack[:len(stack)-1]
		state[modulePath] = 2
		if !valid {
			return false
		}
		graph.Packages[modulePath] = header
		graph.Order = append(graph.Order, modulePath)
		return true
	}
	for _, root := range normalizedRoots {
		visit(root, source.Span{})
	}
	graph.Hash = hashHeaderGraph(normalizedRoots, normalizedTarget, graph.Packages, graph.Order)
	graph.Diagnostics = collector.Diagnostics()
	return graph, nil
}

func hashHeaderGraph(roots []string, buildTarget target.Target, packages map[string]PackageHeader, order []string) string {
	hasher := sha256.New()
	hasher.Write([]byte(GraphFormat))
	hasher.Write([]byte{0})
	hasher.Write([]byte(strconv.Itoa(GraphVersion)))
	hasher.Write([]byte{0})
	for _, root := range roots {
		hasher.Write([]byte(root))
		hasher.Write([]byte{0})
	}
	for _, tag := range buildTarget.Tags {
		hasher.Write([]byte(tag))
		hasher.Write([]byte{0})
	}
	for _, modulePath := range order {
		pkg := packages[modulePath]
		hasher.Write([]byte(modulePath))
		hasher.Write([]byte{0})
		hasher.Write([]byte(pkg.Source.ID.String()))
		hasher.Write([]byte{0})
		for _, candidate := range pkg.Candidates {
			hasher.Write([]byte(candidate.Path))
			hasher.Write([]byte{0})
			hasher.Write([]byte(candidate.Hash))
			hasher.Write([]byte{0})
			if candidate.Selected {
				hasher.Write([]byte{1})
			} else {
				hasher.Write([]byte{0})
			}
		}
		for _, resource := range pkg.Source.Resources {
			hasher.Write([]byte(resource.Path))
			hasher.Write([]byte{0})
			hasher.Write([]byte(resource.Hash))
			hasher.Write([]byte{0})
		}
		for _, dependency := range pkg.Imports {
			hasher.Write([]byte(dependency))
			hasher.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// HashHeaderGraph returns the canonical identity of an already resolved source
// graph. Callers may use it after adding dependencies discovered during lowering.
func HashHeaderGraph(roots []string, buildTarget target.Target, packages map[string]PackageHeader, order []string) string {
	return hashHeaderGraph(roots, buildTarget, packages, order)
}
