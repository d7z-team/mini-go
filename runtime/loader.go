package runtime

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type executable struct {
	Artifact        ir.Artifact
	Hash            string
	Types           map[string]types.TypeNode
	Functions       map[string]loadedFunction
	FunctionOrder   []loadedFunction
	FunctionIndexes map[string]int
	Exports         map[string]ir.Export
	Constants       map[string]int
	Globals         map[string]int
	StorageTypeKeys []string
}

func (e *executable) hasType(name string) bool {
	if e == nil {
		return false
	}
	_, ok := e.Types[strings.TrimSpace(name)]
	return ok
}

func (e *executable) typeShape(node types.TypeNode) (types.TypeNode, bool) {
	if e == nil {
		return types.TypeNode{}, false
	}
	return e.Artifact.TypeTable.Node(e.Artifact.TypeTable.Underlying(types.Ref(node)))
}

func (e *executable) typeFields(node types.TypeNode) []types.Field {
	shape, ok := e.typeShape(node)
	if !ok || shape.Kind != types.Struct {
		return nil
	}
	return shape.Fields
}

type loader struct {
	Limits ir.ValidationLimits
}

type loadedFunction struct {
	Decl             ir.Function
	Instructions     []preparedInstruction
	LocalIndexes     map[string]int
	UpvalueIndexes   map[string]int
	ResultTypes      []vmType
	ResultLayouts    []preparedValueLayout
	LocalTypes       []vmType
	LocalLayouts     []preparedValueLayout
	LocalVariadic    []bool
	LocalEscapes     []bool
	TemporaryTypes   []vmType
	TemporaryLayouts []preparedValueLayout
	// FrameValueBytes excludes captured cells, whose count is checked when the
	// closure is bound. All declared local, temporary and result costs are fixed.
	FrameValueBytes int64
	// These facts classify storage only. Liveness remains a property of the
	// current instruction and frame state.
	LocalReferences []bool
	SlotReferences  []bool
	ReferenceCells  []bool
	ReferenceMaps   [][]uint64
	ReferenceAtPC   []uint32
}

func newLoader() *loader {
	return &loader{Limits: ir.DefaultValidationLimits()}
}

func (l *loader) load(artifact ir.Artifact) (*executable, error) {
	if err := ir.ValidateArtifactWithLimits(&artifact, l.validationLimits()); err != nil {
		return nil, err
	}
	return l.loadValidated(artifact)
}

func (l *loader) loadValidated(artifact ir.Artifact) (*executable, error) {
	if err := artifact.TypeTable.Reindex(); err != nil {
		return nil, err
	}
	hash, err := ir.HashValidated(&artifact)
	if err != nil {
		return nil, err
	}
	constantIndexes := make(map[string]int, len(artifact.Constants))
	for i, constant := range artifact.Constants {
		constantIndexes[constant.ID] = i
	}
	globalIndexes := make(map[string]int, len(artifact.Globals))
	for i, global := range artifact.Globals {
		globalIndexes[global.ID] = i
	}
	functions := make(map[string]loadedFunction, len(artifact.Functions))
	functionOrder := make([]loadedFunction, 0, len(artifact.Functions))
	functionIndexes := make(map[string]int, len(artifact.Functions))
	// Every reference here belongs to the same artifact table. Reuse its
	// immutable plan across functions; the lookup itself ends with loading.
	layouts := make(map[types.TypeRef]preparedValueLayout)
	prepareTypeLayout := func(typ vmType) preparedValueLayout {
		if layout, ok := layouts[typ.Ref]; ok {
			return layout
		}
		layout, _ := buildPreparedValueLayout(typ, map[string]bool{}, nil)
		layouts[typ.Ref] = layout
		return layout
	}
	for _, fn := range artifact.Functions {
		operations, err := fn.Operations()
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", fn.ID, err)
		}
		resultTypes := make([]vmType, len(fn.Signature.Results))
		for i, result := range fn.Signature.Results {
			resultTypes[i] = runtimeTypeWithTable(result, &artifact.TypeTable)
			resultTypes[i].text = types.FormatWithTable(&artifact.TypeTable, result)
		}
		localIndexes := make(map[string]int, len(fn.Locals))
		localTypes := make([]vmType, len(fn.Locals))
		localVariadic := make([]bool, len(fn.Locals))
		localEscapes := make([]bool, len(fn.Locals))
		localReferences := make([]bool, len(fn.Locals))
		localLayouts := make([]preparedValueLayout, len(fn.Locals))
		for i, local := range fn.Locals {
			localIndexes[local.ID] = i
			localTypes[i] = runtimeTypeWithTable(local.Type, &artifact.TypeTable)
			localTypes[i].text = types.FormatWithTable(&artifact.TypeTable, local.Type)
			localLayouts[i] = prepareTypeLayout(localTypes[i])
			localReferences[i] = runtimeTypeMayOwnStorage(localTypes[i])
			if signature, ok := artifact.TypeTable.IsFunction(local.Type); ok {
				localVariadic[i] = signature.Variadic
			}
		}
		upvalueIndexes := make(map[string]int, len(fn.Upvalues))
		for i, upvalue := range fn.Upvalues {
			upvalueIndexes[upvalue.ID] = i
		}
		resultLayouts := make([]preparedValueLayout, len(resultTypes))
		for index, typ := range resultTypes {
			resultLayouts[index] = prepareTypeLayout(typ)
		}
		temporaryTypes := make([]vmType, len(fn.Code.Types))
		temporaryLayouts := make([]preparedValueLayout, len(fn.Code.Types))
		for index, ref := range fn.Code.Types {
			temporaryTypes[index] = runtimeTypeWithTable(ref, &artifact.TypeTable)
			temporaryTypes[index].text = types.FormatWithTable(&artifact.TypeTable, ref)
			temporaryLayouts[index] = prepareTypeLayout(temporaryTypes[index])
		}
		labels, _, err := instructionLayout(fn)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", fn.ID, err)
		}
		instructions := make([]preparedInstruction, 0, len(operations)-len(labels))
		for i, inst := range operations {
			if inst.Op == ir.OpLabel {
				continue
			}
			prepared, err := prepareInstruction(inst)
			if err != nil {
				return nil, fmt.Errorf("function %s instruction %d: %w", fn.ID, i, err)
			}
			prepared.bindTypes(&artifact.TypeTable, artifact.Module.Path)
			prepared.operands = &fn.Code.Operands[fn.Code.Instructions[i].Operands]
			if prepared.op == preparedUnary || prepared.op == preparedBinary || prepared.op == preparedCompareBranch {
				for index, input := range prepared.operands.Inputs {
					var typ types.TypeRef
					switch input.Kind {
					case ir.OperandConstant:
						typ = artifact.Constants[input.Index].Type
					case ir.OperandLocal:
						typ = fn.Locals[input.Index].Type
					default:
						typ = fn.Code.Types[input.Index]
					}
					if primitive, ok := types.View(&artifact.TypeTable, typ).Primitive(); ok && primitive >= types.PrimitiveInt && primitive <= types.PrimitiveComplex128 {
						prepared.numericInputs[index] = primitive
					}
					if index == 0 && (prepared.op == preparedBinary || prepared.op == preparedCompareBranch) {
						prepared.runtimeType = runtimeTypeWithTable(typ, &artifact.TypeTable)
					}
				}
			}
			if prepared.constant != nil {
				prepared.constantIndex = constantIndexes[prepared.constant.Constant]
			}
			if prepared.local != nil {
				prepared.localIndex = localIndexes[prepared.local.Local]
			}
			if prepared.upvalue != nil {
				prepared.upvalueIndex = upvalueIndexes[prepared.upvalue.Upvalue]
			}
			if prepared.global != nil {
				prepared.globalIndex = globalIndexes[prepared.global.Global]
			}
			if prepared.jump != nil {
				prepared.jumpPC = labels[prepared.jump.Label]
			}
			if prepared.comparison != nil {
				prepared.jumpPC = labels[prepared.comparison.Label]
			}
			if payload := prepared.typeDispatch; payload != nil {
				dispatch := &preparedTypeSwitch{subject: localIndexes[payload.Subject], fallback: labels[payload.Default], fallbackLocal: -1}
				if payload.DefaultLocal != "" {
					dispatch.fallbackLocal = localIndexes[payload.DefaultLocal]
				}
				for _, match := range payload.Cases {
					entry := preparedTypeCase{typ: runtimeTypeWithTable(match.Type, &artifact.TypeTable), target: labels[match.Label], binding: -1, original: match.Original}
					if match.Binding != "" {
						entry.binding = localIndexes[match.Binding]
					}
					if dispatch.concreteEnd == len(dispatch.cases) {
						if key, ok := typeDispatchIdentityOf(entry.typ); ok {
							if dispatch.concrete == nil {
								dispatch.concrete = make(map[typeDispatchIdentity]int)
							}
							if _, exists := dispatch.concrete[key]; !exists {
								dispatch.concrete[key] = len(dispatch.cases)
							}
							dispatch.concreteEnd++
						}
					}
					dispatch.cases = append(dispatch.cases, entry)
				}
				prepared.dispatch = dispatch
			}
			if prepared.address != nil && prepared.address.Kind == "local" {
				if index, ok := localIndexes[prepared.address.Local]; ok {
					localEscapes[index] = true
				}
			}
			if prepared.closure != nil {
				for _, capture := range prepared.closure.Captures {
					if capture.Kind != "local" {
						continue
					}
					if index, ok := localIndexes[capture.Local]; ok {
						localEscapes[index] = true
					}
				}
			}
			instructions = append(instructions, prepared)
		}
		loaded := loadedFunction{
			Decl:             fn,
			Instructions:     instructions,
			LocalIndexes:     localIndexes,
			UpvalueIndexes:   upvalueIndexes,
			ResultTypes:      resultTypes,
			ResultLayouts:    resultLayouts,
			LocalTypes:       localTypes,
			LocalLayouts:     localLayouts,
			LocalVariadic:    localVariadic,
			LocalEscapes:     localEscapes,
			TemporaryTypes:   temporaryTypes,
			TemporaryLayouts: temporaryLayouts,
			LocalReferences:  localReferences,
			SlotReferences:   runtimeTypeReferenceMask(fn.Code.Types, &artifact.TypeTable),
		}
		loaded.FrameValueBytes = preparedFrameLogicalBytes(nil, loaded, 0)
		storageCells := make([]bool, len(localTypes))
		for index := range localTypes {
			if directFrameLocal(localTypes[index]) {
				continue
			}
			storageCells[index] = localLayouts[index].Stride == 0 || !inlineFrameLayout(localLayouts[index])
		}
		references, err := ir.PrepareFrameReferences(&fn, loaded.LocalReferences, loaded.SlotReferences, storageCells)
		if err != nil {
			return nil, fmt.Errorf("function %s reference plan: %w", fn.ID, err)
		}
		_, executionPCs, err := instructionLayout(fn)
		if err != nil {
			return nil, fmt.Errorf("function %s reference layout: %w", fn.ID, err)
		}
		loaded.ReferenceCells = references.Cells
		loaded.ReferenceMaps = make([][]uint64, len(instructions)+1)
		loaded.ReferenceAtPC = make([]uint32, len(instructions)+1)
		for wirePC, execPC := range executionPCs {
			if execPC < 0 || execPC >= len(loaded.ReferenceAtPC) || wirePC >= len(references.AtPC) {
				continue
			}
			mapID := references.AtPC[wirePC]
			loaded.ReferenceAtPC[execPC] = mapID
			if int(mapID) < len(references.Maps) {
				loaded.ReferenceMaps[execPC] = references.Maps[mapID]
			}
		}
		functions[fn.ID] = loaded
		functionIndexes[fn.ID] = len(functionOrder)
		functionOrder = append(functionOrder, loaded)
	}
	namedTypes := artifact.TypeTable.DeclaredNamed(artifact.Module.Path)
	typeIndex := make(map[string]types.TypeNode, len(namedTypes))
	for _, typ := range namedTypes {
		typeIndex[string(typ.Identity.DeclID)] = typ
	}
	exports := make(map[string]ir.Export, len(artifact.Exports))
	for _, export := range artifact.Exports {
		exports[export.Name] = export
	}
	loaded := &executable{
		Artifact:  artifact,
		Hash:      hash,
		Types:     typeIndex,
		Functions: functions, FunctionOrder: functionOrder, FunctionIndexes: functionIndexes,
		Exports:   exports,
		Constants: constantIndexes,
		Globals:   globalIndexes,
	}
	// TypeTable indexes belong to its address. Prepare the final copy before
	// publishing the executable to instances and concurrent tasks.
	if err := loaded.Artifact.TypeTable.Reindex(); err != nil {
		return nil, err
	}
	// Code owns the static type descriptors it can use in future activations.
	// Resolve those keys once here; collection only marks existing registry
	// entries and never rebuilds or formats the executable's type graph.
	seenStorageTypes := make(map[string]bool)
	addStorageType := func(ref types.TypeRef) {
		if !ref.Valid() {
			return
		}
		key := types.FormatWithTable(&loaded.Artifact.TypeTable, ref)
		if !seenStorageTypes[key] {
			seenStorageTypes[key] = true
			loaded.StorageTypeKeys = append(loaded.StorageTypeKeys, key)
		}
	}
	for _, node := range artifact.TypeTable.Nodes {
		addStorageType(types.Ref(node))
	}
	for _, constant := range artifact.Constants {
		addStorageType(constant.Type)
	}
	for _, global := range artifact.Globals {
		addStorageType(global.Type)
	}
	for _, fn := range functionOrder {
		for _, typ := range fn.LocalTypes {
			addStorageType(typ.Ref)
		}
		for _, typ := range fn.TemporaryTypes {
			addStorageType(typ.Ref)
		}
		for _, typ := range fn.ResultTypes {
			addStorageType(typ.Ref)
		}
		for _, upvalue := range fn.Decl.Upvalues {
			addStorageType(upvalue.Type)
		}
		for _, instruction := range fn.Instructions {
			addStorageType(instruction.runtimeType.Ref)
		}
	}
	return loaded, nil
}

func (inst *preparedInstruction) bindTypes(table *types.TypeTable, modulePath string) {
	if inst == nil || table == nil {
		return
	}
	var ref types.TypeRef
	switch {
	case inst.typeOperand != nil:
		ref = inst.typeOperand.Type
	case inst.minMax != nil:
		ref = inst.minMax.Type
	case inst.makeSequence != nil:
		ref = inst.makeSequence.Type
	case inst.makeMap != nil:
		ref = inst.makeMap.Type
	case inst.makeStruct != nil:
		ref = inst.makeStruct.Type
	case inst.makeSlice != nil:
		ref = inst.makeSlice.Type
	case inst.makeWaitable != nil:
		ref = inst.makeWaitable.Type
	}
	if ref.Valid() {
		inst.runtimeType = runtimeTypeWithTable(ref, table)
		inst.runtimeType.text = types.FormatWithTable(table, ref)
		if inst.op == preparedConvert || inst.op == preparedIndexConvert {
			inst.conversion = conversionMetadataFor(inst.runtimeType)
		}
		if signature, ok := table.IsFunction(ref); ok {
			inst.typeVariadic = signature.Variadic
		}
		if inst.runtimeType.ShapeKind() == types.Struct {
			inst.structSchema = standaloneStructSchema(inst.runtimeType)
		}
		if inst.minMax != nil {
			if inst.runtimeType.Primitive(types.PrimitiveString) {
				inst.minMaxKind = minMaxString
			} else if numeric, ok := inst.runtimeType.NumericInfo(); ok {
				switch numeric.Kind {
				case types.NumericSigned:
					inst.minMaxKind = minMaxSigned
				case types.NumericUnsigned:
					inst.minMaxKind = minMaxUnsigned
				case types.NumericFloat:
					inst.minMaxKind = minMaxFloat
				}
			}
		}
	}
	if inst.call != nil {
		inst.callModule = strings.TrimSpace(inst.call.ModulePath)
		if inst.callModule == "" {
			inst.callModule = strings.TrimSpace(modulePath)
		}
	}
	if inst.fieldPath != nil {
		current := inst.fieldPath.Type
		for _, index := range inst.fieldPath.Fields {
			view := types.View(table, current)
			step := preparedField{}
			if view.Shape() == types.Pointer {
				step.indirect = true
				element, _ := view.Elem()
				view = types.View(table, element)
			}
			fields, _ := view.StructFields()
			for i := uint32(0); i < index; i++ {
				if fields[i].Name != "_" && fields[i].Name != "" {
					step.index++
				}
			}
			current = fields[index].Type
			step.typ = runtimeTypeWithTable(current, table)
			inst.path = append(inst.path, step)
		}
	}
}

func (l *loader) validationLimits() ir.ValidationLimits {
	if l == nil {
		return ir.DefaultValidationLimits()
	}
	return l.Limits
}

func (l *loader) loadJSON(data []byte) (*executable, error) {
	artifact, err := ir.ReadJSONWithLimits(bytes.NewReader(data), l.validationLimits())
	if err != nil {
		return nil, err
	}
	return l.loadValidated(artifact)
}

func instructionLayout(fn ir.Function) (map[string]int, []int, error) {
	operations, err := fn.Operations()
	if err != nil {
		return nil, nil, err
	}
	labels := make(map[string]int)
	executionPCs := make([]int, len(operations))
	pc := 0
	for i, inst := range operations {
		executionPCs[i] = pc
		if inst.Op != ir.OpLabel {
			pc++
			continue
		}
		var payload ir.LabelPayload
		if err := ir.ReadInstructionPayload(inst.Payload, &payload); err != nil {
			return nil, nil, fmt.Errorf("instruction %d label payload: %w", i, err)
		}
		if payload.Label == "" {
			return nil, nil, fmt.Errorf("instruction %d label is empty", i)
		}
		labels[payload.Label] = pc
	}
	return labels, executionPCs, nil
}
