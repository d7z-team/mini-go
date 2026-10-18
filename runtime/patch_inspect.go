package runtime

import (
	"errors"
	"reflect"
	"sort"
)

type DeclarationChange struct {
	Name string
	Kind string // added, removed, modified
}

type ModuleChange struct {
	Path             string
	Kind             string
	Functions        []DeclarationChange
	Globals          []DeclarationChange
	Types            []DeclarationChange
	Exports          []DeclarationChange
	ConstantsChanged bool
	TypeTableChanged bool
}

// PatchImpact describes representation changes and structural compatibility.
// It does not predict behavior or instance admission, and owns no VM references.
type PatchImpact struct {
	Base                ProgramIdentity
	Target              ProgramIdentity
	BaseGeneration      uint64 // set by PatchPlan.Inspect
	Compatible          bool
	Rejection           *PatchError
	SymbolsChanged      bool
	Modules             []ModuleChange
	Entries             []DeclarationChange
	AddedCapabilities   []string
	RemovedCapabilities []string
}

// ComparePrograms inspects even incompatible candidates without entering a VM.
func ComparePrograms(base, target *Program) (PatchImpact, error) {
	var out PatchImpact
	if base == nil || base.code == nil || target == nil || target.code == nil {
		return out, errors.New("base and target programs are required")
	}
	out.Base, out.Target = base.Identity(), target.Identity()
	out.SymbolsChanged = out.Base.SymbolsHash != out.Target.SymbolsHash
	_, err := buildPatchPlan(&instanceRevision{code: base.code, symbols: base.symbols}, target)
	var reason PatchError
	isPatchError := errors.As(err, &reason)
	out.Compatible = err == nil || reason.Code == "unchanged"
	if !out.Compatible {
		if isPatchError {
			out.Rejection = &reason
		} else {
			return out, err
		}
	}
	paths := make(map[string]bool, len(base.code.modules)+len(target.code.modules))
	for path := range base.code.modules {
		paths[path] = true
	}
	for path := range target.code.modules {
		paths[path] = true
	}
	for path := range paths {
		before, after := base.code.modules[path], target.code.modules[path]
		change := ModuleChange{Path: path, Kind: "modified"}
		if before == nil {
			change.Kind = "added"
		}
		if after == nil {
			change.Kind = "removed"
		}
		if before != nil && after != nil && reflect.DeepEqual(before.Artifact, after.Artifact) {
			continue
		}
		oldGroups, newGroups := patchDeclarations(before), patchDeclarations(after)
		change.Functions = compareDeclarations(oldGroups[0], newGroups[0])
		change.Globals = compareDeclarations(oldGroups[1], newGroups[1])
		change.Types = compareDeclarations(oldGroups[2], newGroups[2])
		change.Exports = compareDeclarations(oldGroups[3], newGroups[3])
		if before == nil || after == nil {
			change.ConstantsChanged = true
			change.TypeTableChanged = true
		} else {
			change.ConstantsChanged = !reflect.DeepEqual(before.Artifact.Constants, after.Artifact.Constants)
			change.TypeTableChanged = !reflect.DeepEqual(before.Artifact.TypeTable, after.Artifact.TypeTable)
		}
		out.Modules = append(out.Modules, change)
	}
	sort.Slice(out.Modules, func(a, b int) bool { return out.Modules[a].Path < out.Modules[b].Path })
	oldEntries, newEntries := make(map[string]any), make(map[string]any)
	for _, entry := range base.code.image.Entries {
		oldEntries[entry.Name] = entry
	}
	for _, entry := range target.code.image.Entries {
		newEntries[entry.Name] = entry
	}
	out.Entries = compareDeclarations(oldEntries, newEntries)
	oldCaps, newCaps := make(map[string]any), make(map[string]any)
	for _, capability := range base.RequiredHostCapabilities() {
		oldCaps[capability] = true
	}
	for _, capability := range target.RequiredHostCapabilities() {
		newCaps[capability] = true
	}
	for _, change := range compareDeclarations(oldCaps, newCaps) {
		if change.Kind == "added" {
			out.AddedCapabilities = append(out.AddedCapabilities, change.Name)
		} else {
			out.RemovedCapabilities = append(out.RemovedCapabilities, change.Name)
		}
	}
	return out, nil
}

func patchDeclarations(module *executable) [4]map[string]any {
	groups := [4]map[string]any{{}, {}, {}, {}}
	if module == nil {
		return groups
	}
	for _, function := range module.Artifact.Functions {
		groups[0][function.ID] = function
	}
	for _, global := range module.Artifact.Globals {
		groups[1][global.ID] = global
	}
	for _, declaration := range module.Artifact.TypeTable.DefinedNamed(module.Artifact.Module.Path) {
		groups[2][string(declaration.Identity.DeclID)] = declaration
	}
	for _, export := range module.Artifact.Exports {
		groups[3][export.Name] = export
	}
	return groups
}

func compareDeclarations(before, after map[string]any) []DeclarationChange {
	var changes []DeclarationChange
	for name, value := range before {
		next, ok := after[name]
		if !ok {
			changes = append(changes, DeclarationChange{Name: name, Kind: "removed"})
		} else if !reflect.DeepEqual(value, next) {
			changes = append(changes, DeclarationChange{Name: name, Kind: "modified"})
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			changes = append(changes, DeclarationChange{Name: name, Kind: "added"})
		}
	}
	sort.Slice(changes, func(a, b int) bool { return changes[a].Name < changes[b].Name })
	return changes
}

// Inspect copies immutable inputs under the plan lock, then compares outside it.
// Closed or committed plans no longer own their inspection inputs.
func (p *PatchPlan) Inspect() (PatchImpact, error) {
	if p == nil {
		return PatchImpact{}, errors.New("nil patch plan")
	}
	p.mu.Lock()
	base, target, generation := p.base, p.target, p.baseGeneration
	p.mu.Unlock()
	out, err := ComparePrograms(base, target)
	out.BaseGeneration = generation
	return out, err
}
