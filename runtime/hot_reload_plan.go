package runtime

import (
	"slices"
	"sort"
)

func buildPatchPlan(current *instanceRevision, next *Program) (*PatchPlan, error) {
	if current == nil || current.code == nil || next == nil || next.code == nil {
		return nil, PatchError{Code: "invalid_program", Message: "current and target programs are required"}
	}
	if current.code.image.Hash == next.code.image.Hash && current.symbolsHash() == next.SymbolsHash() {
		return nil, PatchError{Code: "unchanged", Message: "target program is already current"}
	}
	if current.code.image.Hash == next.code.image.Hash {
		return &PatchPlan{base: &Program{code: current.code, symbols: current.symbols}, baseHash: current.code.image.Hash, target: next}, nil
	}
	if current.code.image.Root != next.code.image.Root {
		return nil, PatchError{Code: "root_changed", Message: "root module changed"}
	}
	if !slices.Equal(current.code.image.Target.Tags, next.code.image.Target.Tags) {
		return nil, PatchError{Code: "target_changed", Message: "build target changed"}
	}
	changed := make([]string, 0, len(next.code.modules))
	for path, newModule := range next.code.modules {
		oldModule := current.code.modules[path]
		if oldModule == nil || oldModule.Hash != newModule.Hash {
			changed = append(changed, path)
		}
	}
	for path := range current.code.modules {
		if _, exists := next.code.modules[path]; !exists {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return &PatchPlan{
		base:           &Program{code: current.code, symbols: current.symbols},
		baseHash:       current.code.image.Hash,
		target:         next,
		changedModules: changed,
	}, nil
}
