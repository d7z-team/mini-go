package runtime

import (
	"errors"
	"sync"
)

type instanceRevision struct {
	mu           sync.Mutex
	owner        *vm
	generation   uint64
	code         *programCode
	symbols      *symbolIndex
	root         *moduleInstance
	modules      *moduleRegistry
	moduleOrder  []*moduleInstance
	entries      map[string]string
	entry        string
	pins         int
	rootRetained bool
	retired      bool
	closed       bool
}

func (revision *instanceRevision) retain() bool {
	if revision == nil {
		return false
	}
	revision.mu.Lock()
	if revision.closed {
		revision.mu.Unlock()
		return false
	}
	revision.pins++
	revision.mu.Unlock()
	return true
}

func (revision *instanceRevision) release() {
	if revision == nil {
		return
	}
	revision.mu.Lock()
	if revision.pins > 0 {
		revision.pins--
	}
	revision.closeReadyLocked()
	closed := revision.closed
	revision.mu.Unlock()
	if closed && revision.owner != nil {
		revision.owner.removeRetiredRevision(revision)
	}
}

func (revision *instanceRevision) closeReadyLocked() {
	if revision == nil || revision.closed || !revision.retired || revision.rootRetained || revision.pins != 0 {
		return
	}
	revision.closed = true
	revision.releasePayloadLocked()
}

func (revision *instanceRevision) retire(rootRetained bool) {
	if revision == nil {
		return
	}
	revision.mu.Lock()
	revision.retired = true
	revision.rootRetained = rootRetained
	revision.closeReadyLocked()
	revision.mu.Unlock()
}

func (revision *instanceRevision) setRootRetained(retained bool) {
	if revision == nil {
		return
	}
	revision.mu.Lock()
	revision.rootRetained = retained
	revision.closeReadyLocked()
	revision.mu.Unlock()
}

func (revision *instanceRevision) close() {
	if revision == nil {
		return
	}
	revision.mu.Lock()
	if revision.closed {
		revision.mu.Unlock()
		return
	}
	revision.closed = true
	revision.releasePayloadLocked()
	revision.mu.Unlock()
}

func (revision *instanceRevision) releasePayloadLocked() {
	for _, module := range revision.moduleOrder {
		if module == nil {
			continue
		}
		module.framePoolMu.Lock()
		if module.framePoolPhysicalBytes != 0 {
			if module.vm != nil {
				module.vm.idleFrameBytes.Add(-module.framePoolPhysicalBytes)
			}
			module.framePoolPhysicalBytes = 0
			module.framePoolBytes = 0
			module.framePools = nil
		}
		module.framePoolMu.Unlock()
	}
	revision.code = nil
	revision.symbols = nil
	revision.root = nil
	revision.modules = nil
	clear(revision.moduleOrder)
	revision.moduleOrder = nil
	clear(revision.entries)
	revision.entries = nil
}

func (vm *vm) installInitialRevision(program *Program) {
	code := program.code
	entryFunction := ""
	if entry, ok := code.image.DefaultEntry(); ok {
		entryFunction = code.entries[entry.Name]
	}
	revision := &instanceRevision{
		owner: vm, generation: 1, code: code, symbols: program.symbols, root: vm.rootModule(), modules: vm.moduleRegistry(),
		moduleOrder: make([]*moduleInstance, len(code.moduleOrder)), entries: cloneEntries(code.entries), entry: entryFunction,
	}
	for _, module := range revision.modules.modules {
		module.revision = revision
	}
	for index, path := range code.moduleOrder {
		revision.moduleOrder[index], _ = revision.modules.module(path)
	}
	vm.revision.Store(revision)
}

func newInstanceRevision(program *Program, previous *instanceRevision, generation uint64) (*instanceRevision, error) {
	code := program.code
	registry := newModuleRegistry()
	entryFunction := ""
	if entry, ok := code.image.DefaultEntry(); ok {
		entryFunction = code.entries[entry.Name]
	}
	revision := &instanceRevision{
		owner: previous.owner, generation: generation, code: code, symbols: program.symbols, modules: registry,
		moduleOrder: make([]*moduleInstance, len(code.moduleOrder)), entries: cloneEntries(code.entries), entry: entryFunction,
	}
	for index, path := range code.moduleOrder {
		executable := code.modules[path]
		var module *moduleInstance
		if oldModule, ok := previous.modules.module(path); ok {
			var err error
			module, err = bindModuleState(executable, oldModule.state)
			if err != nil {
				return nil, err
			}
		} else {
			module = newModuleInstance(executable)
		}
		module.revision = revision
		module.vm = previous.root.vm
		if err := registry.addModule(module); err != nil {
			return nil, err
		}
		revision.moduleOrder[index] = module
		if path == code.image.Root {
			revision.root = module
		}
	}
	if revision.root == nil {
		return nil, errors.New("target revision has no root module")
	}
	return revision, nil
}

func (revision *instanceRevision) rebindGlobalSlots() {
	for _, module := range revision.modules.modules {
		for _, global := range module.executable.Artifact.Globals {
			cell := module.state.globals[global.ID]
			cell.module = module
			cell.typ = module.runtimeType(global.Type)
			_, cell.variadic, _ = module.functionTypeInfo(global.Type)
		}
	}
}

func cloneEntries(entries map[string]string) map[string]string {
	out := make(map[string]string, len(entries))
	for name, function := range entries {
		out[name] = function
	}
	return out
}

// Revision returns the revision currently used for new calls.
func (i *Instance) Revision() RevisionInfo {
	if i == nil || i.vm == nil {
		return RevisionInfo{}
	}
	revision := i.vm.revision.Load()
	if revision == nil || revision.code == nil {
		return RevisionInfo{}
	}
	return RevisionInfo{Generation: revision.generation, Hash: revision.code.image.Hash, SymbolsHash: revision.symbolsHash()}
}

func (revision *instanceRevision) symbolsHash() string {
	if revision == nil || revision.symbols == nil {
		return ""
	}
	return revision.symbols.hash
}
