package minigo

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/stdlib"
)

type libraryKind uint8

const (
	standardLibrary libraryKind = iota + 1
	moduleLibrary
)

// Library is one immutable source provider registered by an embedder.
type Library struct {
	id                  string
	kind                libraryKind
	modulePath          string
	sources             workspace.SourceSet
	capabilities        []HostCapability
	packageCapabilities map[string][]HostCapability
}

// HostCapability identifies one host service required by registered source.
type HostCapability = stdlib.HostCapability

// NewStandardLibrary registers packages below sources as bare standard-library
// import paths. Multiple registered libraries may add files to the same package.
func NewStandardLibrary(id string, sources fs.FS, capabilities ...HostCapability) (Library, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Library{}, errors.New("standard library ID is empty")
	}
	if sources == nil {
		return Library{}, errors.New("standard library sources are nil")
	}
	sourceSet, err := workspace.StandardLibrary(sources)
	if err != nil {
		return Library{}, err
	}
	seen := make(map[HostCapability]struct{}, len(capabilities))
	normalized := make([]HostCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = HostCapability(strings.TrimSpace(string(capability)))
		if capability == "" {
			return Library{}, errors.New("standard library capability is empty")
		}
		if _, exists := seen[capability]; exists {
			return Library{}, fmt.Errorf("duplicate standard library capability %q", capability)
		}
		seen[capability] = struct{}{}
		normalized = append(normalized, capability)
	}
	slices.Sort(normalized)
	paths, err := sourceSet.PackagePaths()
	if err != nil {
		return Library{}, err
	}
	byPackage := make(map[string][]HostCapability, len(paths))
	for _, path := range paths {
		byPackage[path] = append([]HostCapability(nil), normalized...)
	}
	return Library{id: id, kind: standardLibrary, sources: sourceSet, capabilities: normalized, packageCapabilities: byPackage}, nil
}

// NewModuleLibrary registers a source tree below a logical import prefix.
// Its filesystem must remain immutable while an Engine uses the LibrarySet.
func NewModuleLibrary(id, modulePath string, sources fs.FS) (Library, error) {
	id = strings.TrimSpace(id)
	modulePath = strings.TrimSpace(modulePath)
	if id == "" {
		return Library{}, errors.New("module library ID is empty")
	}
	if sources == nil {
		return Library{}, errors.New("module library sources are nil")
	}
	sourceSet, err := workspace.DiscoverIndexedSourceTree(sources, ".", modulePath)
	if err != nil {
		return Library{}, err
	}
	return Library{id: id, kind: moduleLibrary, modulePath: modulePath, sources: sourceSet}, nil
}

// LibrarySet is an immutable collection of embedder-provided source libraries.
type LibrarySet struct {
	standards           []Library
	modules             []Library
	capabilities        []HostCapability
	packageCapabilities map[string][]HostCapability
}

// NewLibrarySet validates and freezes the libraries available to an Engine.
func NewLibrarySet(libraries ...Library) (*LibrarySet, error) {
	set := &LibrarySet{packageCapabilities: make(map[string][]HostCapability)}
	ids := make(map[string]struct{}, len(libraries))
	modules := make(map[string]bool)
	for _, library := range libraries {
		if strings.TrimSpace(library.id) == "" {
			return nil, errors.New("registered library has an empty ID")
		}
		if _, exists := ids[library.id]; exists {
			return nil, fmt.Errorf("duplicate registered library ID %q", library.id)
		}
		ids[library.id] = struct{}{}
		switch library.kind {
		case standardLibrary:
			if library.sources == nil {
				return nil, fmt.Errorf("standard library %q has no sources", library.id)
			}
			set.standards = append(set.standards, library)
			set.capabilities = append(set.capabilities, library.capabilities...)
			for path, capabilities := range library.packageCapabilities {
				set.packageCapabilities[path] = append(set.packageCapabilities[path], capabilities...)
			}
		case moduleLibrary:
			if library.sources == nil {
				return nil, fmt.Errorf("module library %q has no sources", library.id)
			}
			if modules[library.modulePath] {
				return nil, fmt.Errorf("module %q is registered more than once", library.modulePath)
			}
			modules[library.modulePath] = true
			set.modules = append(set.modules, library)
		default:
			return nil, fmt.Errorf("registered library %q has invalid kind %d", library.id, library.kind)
		}
	}
	slices.SortFunc(set.modules, func(a, b Library) int { return strings.Compare(a.modulePath, b.modulePath) })
	slices.SortFunc(set.standards, func(a, b Library) int { return strings.Compare(a.id, b.id) })
	slices.Sort(set.capabilities)
	set.capabilities = slices.Compact(set.capabilities)
	for path, capabilities := range set.packageCapabilities {
		slices.Sort(capabilities)
		set.packageCapabilities[path] = slices.Compact(capabilities)
	}
	return set, nil
}
