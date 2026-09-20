package runtime

import (
	"errors"
	"fmt"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
)

type moduleRegistry struct {
	modules  map[string]*moduleInstance
	revision uint64
}

type moduleInitState uint8

const (
	moduleUninitialized moduleInitState = iota
	moduleInitializing
	moduleReady
	moduleFailed
)

type moduleState struct {
	globals   map[string]*slot
	initState moduleInitState
	initErr   error
	initTask  *executionTask
}

func (s *moduleState) beginInitialization() {
	s.initState = moduleInitializing
	s.initErr = nil
}

func (s *moduleState) finishInitialization(err error) {
	if s.initState != moduleInitializing {
		return
	}
	if err != nil {
		s.initState = moduleFailed
		s.initErr = err
	} else {
		s.initState = moduleReady
	}
	s.initTask = nil
}

type moduleInstance struct {
	executable             *executable
	registry               *moduleRegistry
	revision               *instanceRevision
	vm                     *vm
	state                  *moduleState
	constantMu             sync.Mutex
	constantData           []vmValue
	constantSet            []bool
	globalCells            []*slot
	framePoolMu            sync.Mutex
	framePools             map[string][]*frame
	framePoolBytes         int64
	framePoolPhysicalBytes int64

	localizeTypeCache            metadataCache[string, string]
	qualifyTypeCache             metadataCache[string, string]
	qualifiedTypeCache           metadataCache[string, qualifiedTypeResolution]
	underlyingTypeCache          metadataCache[string, typeTextResolution]
	interfaceTypeCache           metadataCache[string, typeTextResolution]
	typeIdentityCache            metadataCache[string, typeTextResolution]
	runtimeTypeCache             metadataCache[string, vmType]
	runtimeTypes                 metadataCache[types.TypeRef, vmType]
	resolvedRuntimeTypes         metadataCache[types.TypeRef, vmType]
	zeroValueCache               metadataCache[types.TypeRef, vmValue]
	structFieldsCache            metadataCache[string, structFieldsResolution]
	structSchemaCache            metadataCache[string, *structSchema]
	interfaceMethodsCache        metadataCache[string, methodSetResolution]
	declaredMethodSetCache       metadataCache[string, methodSetResolution]
	valueMethodSetCache          metadataCache[string, methodSetResolution]
	methodFunctionCache          metadataCache[string, methodFunctionResolution]
	interfaceImplementationCache metadataCache[string, boolResolution]
	typeInfoCache                metadataCache[types.TypeID, TypeInfo]
	reflectTypeDescriptorCache   metadataCache[string, vmValue]
}

type qualifiedTypeResolution struct {
	module   *moduleInstance
	name     string
	revision uint64
	found    bool
}

type typeTextResolution struct {
	text     string
	revision uint64
	found    bool
}

type structFieldsResolution struct {
	fields []TypeFieldInfo
	found  bool
}

type methodSetResolution struct {
	methods  map[string]string
	revision uint64
}

type methodFunctionResolution struct {
	module       *moduleInstance
	functionID   string
	signature    string
	receiverType string
	revision     uint64
	found        bool
}

type boolResolution struct {
	value    bool
	revision uint64
}

func (m *moduleInstance) formatType(ref types.TypeRef) string {
	return m.runtimeType(ref).String()
}

func (m *moduleInstance) runtimeType(ref types.TypeRef) vmType {
	if m == nil || m.executable == nil {
		return vmType{Ref: ref}
	}
	if runtimeType, ok := m.runtimeTypes.load(ref); ok {
		return runtimeType
	}
	runtimeType := runtimeTypeWithTable(ref, &m.executable.Artifact.TypeTable)
	runtimeType.text = types.FormatWithTable(&m.executable.Artifact.TypeTable, ref)

	m.runtimeTypes.store(ref, runtimeType)
	return runtimeType
}

type functionRef struct {
	ModulePath string
	FunctionID string
	exact      *moduleInstance
	upvalues   map[string]*slot
}

func newModuleRegistry() *moduleRegistry {
	return &moduleRegistry{modules: make(map[string]*moduleInstance)}
}

func (r *moduleRegistry) addExecutable(executable *executable) error {
	if executable == nil {
		return errors.New("nil module executable")
	}
	path := executable.Artifact.Module.Path
	if path == "" {
		return errors.New("module executable missing path")
	}
	if r.modules == nil {
		r.modules = make(map[string]*moduleInstance)
	}
	if _, exists := r.modules[path]; exists {
		return fmt.Errorf("duplicate module %q", path)
	}
	instance := newModuleInstance(executable)
	return r.addModule(instance)
}

func (r *moduleRegistry) addModule(instance *moduleInstance) error {
	if instance == nil || instance.executable == nil {
		return errors.New("nil module")
	}
	path := instance.executable.Artifact.Module.Path
	if path == "" {
		return errors.New("module missing path")
	}
	if r.modules == nil {
		r.modules = make(map[string]*moduleInstance)
	}
	if _, exists := r.modules[path]; exists {
		return fmt.Errorf("duplicate module %q", path)
	}
	instance.registry = r
	r.modules[path] = instance
	r.revision++
	return nil
}

func (r *moduleRegistry) module(path string) (*moduleInstance, bool) {
	if r == nil {
		return nil, false
	}
	module, ok := r.modules[path]
	return module, ok
}

func (r *moduleRegistry) clone() *moduleRegistry {
	out := newModuleRegistry()
	if r == nil {
		return out
	}
	for path, module := range r.modules {
		cloned := module.clone()
		cloned.registry = out
		out.modules[path] = cloned
	}
	out.revision = r.revision
	return out
}

func (m *moduleInstance) clone() *moduleInstance {
	if m == nil {
		return nil
	}
	m.constantMu.Lock()
	constantData := append([]vmValue(nil), m.constantData...)
	constantSet := append([]bool(nil), m.constantSet...)
	m.constantMu.Unlock()
	cloned := &moduleInstance{
		executable:   m.executable,
		registry:     m.registry,
		state:        &moduleState{globals: make(map[string]*slot, len(m.state.globals)), initState: m.state.initState, initErr: m.state.initErr},
		constantData: constantData,
		constantSet:  constantSet,
		globalCells:  make([]*slot, len(m.globalCells)),
	}
	if m.state.initState == moduleInitializing {
		cloned.state.beginInitialization()
	}
	for id, globalSlot := range m.state.globals {
		if globalSlot == nil {
			cloned.state.globals[id] = nil
			continue
		}
		cell := newSlot(globalSlot.typ, cloned, globalSlot.variadic)
		cell.value, cell.initialized = globalSlot.snapshot()
		cloned.state.globals[id] = cell
		if index, ok := m.executable.Globals[id]; ok {
			cloned.globalCells[index] = cell
		}
	}
	return cloned
}

func newModuleInstance(executable *executable) *moduleInstance {
	instance := &moduleInstance{
		executable:   executable,
		state:        &moduleState{globals: make(map[string]*slot, len(executable.Artifact.Globals))},
		constantData: make([]vmValue, len(executable.Artifact.Constants)),
		constantSet:  make([]bool, len(executable.Artifact.Constants)),
		globalCells:  make([]*slot, len(executable.Artifact.Globals)),
		framePools:   make(map[string][]*frame),
	}
	globals := make(map[string]*slot, len(executable.Artifact.Globals))
	for i, global := range executable.Artifact.Globals {
		_, variadic, _ := instance.functionTypeInfo(global.Type)
		cell := newSlot(instance.runtimeType(global.Type), instance, variadic)
		globals[global.ID] = cell
		instance.globalCells[i] = cell
	}
	instance.state.globals = globals
	return instance
}

func bindModuleState(executable *executable, state *moduleState) (*moduleInstance, error) {
	if executable == nil || state == nil {
		return nil, errors.New("nil module code or state")
	}
	bound := newModuleInstance(executable)
	bound.state = state
	bound.globalCells = make([]*slot, len(executable.Artifact.Globals))
	for index, global := range executable.Artifact.Globals {
		cell, ok := state.globals[global.ID]
		if !ok || cell == nil {
			return nil, fmt.Errorf("module state missing global %q", global.ID)
		}
		bound.globalCells[index] = cell
	}
	return bound, nil
}

func (m *moduleInstance) constantValueAt(index int) (vmValue, error) {
	m.constantMu.Lock()
	if m.constantSet[index] {
		value := m.constantData[index]
		m.constantMu.Unlock()
		return value, nil
	}
	m.constantMu.Unlock()
	constant := m.executable.Artifact.Constants[index]
	value, err := m.decodeConstant(m.runtimeType(constant.Type), constant.Value)
	if err != nil {
		return vmValue{}, fmt.Errorf("constant %s: %w", constant.ID, err)
	}
	m.constantMu.Lock()
	defer m.constantMu.Unlock()
	if !m.constantSet[index] {
		m.constantData[index] = value
		m.constantSet[index] = true
	}
	return m.constantData[index], nil
}

func (m *moduleInstance) constantValue(id string) (vmValue, bool, error) {
	if m == nil {
		return vmValue{}, false, errors.New("nil module")
	}
	index, ok := m.executable.Constants[id]
	if !ok {
		return vmValue{}, false, nil
	}
	value, err := m.constantValueAt(index)
	return value, true, err
}
