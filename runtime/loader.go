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
	Decl           ir.Function
	Instructions   []preparedInstruction
	LocalIndexes   map[string]int
	UpvalueIndexes map[string]int
	MaxStack       int
	ResultTypes    []vmType
	LocalTypes     []vmType
	LocalVariadic  []bool
	LocalEscapes   []bool
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
	for _, fn := range artifact.Functions {
		resultTypes := make([]vmType, len(fn.Signature.Results))
		for i, result := range fn.Signature.Results {
			resultTypes[i] = runtimeTypeWithTable(result, &artifact.TypeTable)
			resultTypes[i].text = types.FormatWithTable(&artifact.TypeTable, result)
		}
		localIndexes := make(map[string]int, len(fn.Locals))
		localTypes := make([]vmType, len(fn.Locals))
		localVariadic := make([]bool, len(fn.Locals))
		localEscapes := make([]bool, len(fn.Locals))
		for i, local := range fn.Locals {
			localIndexes[local.ID] = i
			localTypes[i] = runtimeTypeWithTable(local.Type, &artifact.TypeTable)
			localTypes[i].text = types.FormatWithTable(&artifact.TypeTable, local.Type)
			if signature, ok := artifact.TypeTable.IsFunction(local.Type); ok {
				localVariadic[i] = signature.Variadic
			}
		}
		upvalueIndexes := make(map[string]int, len(fn.Upvalues))
		for i, upvalue := range fn.Upvalues {
			upvalueIndexes[upvalue.ID] = i
		}
		labels, _, err := instructionLayout(fn)
		if err != nil {
			return nil, fmt.Errorf("function %s: %w", fn.ID, err)
		}
		instructions := make([]preparedInstruction, 0, len(fn.Instructions)-len(labels))
		for i, inst := range fn.Instructions {
			if inst.Op == string(ir.OpLabel) {
				continue
			}
			prepared, err := prepareInstruction(inst)
			if err != nil {
				return nil, fmt.Errorf("function %s instruction %d: %w", fn.ID, i, err)
			}
			prepared.bindTypes(&artifact.TypeTable, artifact.Module.Path)
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
			Decl:           fn,
			Instructions:   instructions,
			LocalIndexes:   localIndexes,
			UpvalueIndexes: upvalueIndexes,
			MaxStack:       fn.MaxStack,
			ResultTypes:    resultTypes,
			LocalTypes:     localTypes,
			LocalVariadic:  localVariadic,
			LocalEscapes:   localEscapes,
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
		if signature, ok := table.IsFunction(ref); ok {
			inst.typeVariadic = signature.Variadic
		}
		if inst.runtimeType.ShapeKind() == types.Struct {
			inst.structSchema = standaloneStructSchema(inst.runtimeType)
		}
	}
	if inst.call != nil {
		inst.callModule = strings.TrimSpace(inst.call.ModulePath)
		if inst.callModule == "" {
			inst.callModule = strings.TrimSpace(modulePath)
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
	labels := make(map[string]int)
	executionPCs := make([]int, len(fn.Instructions))
	pc := 0
	for i, inst := range fn.Instructions {
		executionPCs[i] = pc
		if inst.Op != string(ir.OpLabel) {
			pc++
			continue
		}
		var payload ir.LabelPayload
		if err := ir.DecodeInstructionPayload(inst.Payload, &payload); err != nil {
			return nil, nil, fmt.Errorf("instruction %d label payload: %w", i, err)
		}
		if payload.Label == "" {
			return nil, nil, fmt.Errorf("instruction %d label is empty", i)
		}
		labels[payload.Label] = pc
	}
	return labels, executionPCs, nil
}
