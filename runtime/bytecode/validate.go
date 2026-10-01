package bytecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func ValidateArtifact(a *Artifact) error {
	return ValidateArtifactWithLimits(a, DefaultValidationLimits())
}

func ValidateArtifactWithLimits(a *Artifact, limits ValidationLimits) error {
	if a == nil {
		return newCodedValidationError(ValidationArtifactNil, "artifact", errors.New("nil artifact"))
	}
	if a.Format != Format {
		return newCodedValidationError(ValidationArtifactFormatUnsupported, "format", fmt.Errorf("unsupported format %q", a.Format))
	}
	if a.Version != CurrentVersion {
		return newCodedValidationError(ValidationArtifactVersionUnsupported, "version", fmt.Errorf("unsupported version %d", a.Version))
	}
	if a.OpcodeSet != OpcodeSet {
		return newCodedValidationError(ValidationArtifactOpcodeUnsupported, "opcode_set", fmt.Errorf("unsupported opcode set %q", a.OpcodeSet))
	}
	if strings.TrimSpace(a.Module.Path) == "" {
		return missingValidationError("module.path", errors.New("missing module path"))
	}
	if strings.TrimSpace(a.Module.Package) == "" {
		return missingValidationError("module.package", errors.New("missing package name"))
	}
	if err := validateArtifactLimits(a, limits); err != nil {
		return err
	}
	if err := a.TypeTable.Validate(); err != nil {
		return newValidationError("type_table", err)
	}
	definedTypes := a.TypeTable.DefinedNamed(a.Module.Path)
	if err := validateUniqueIDs("types", len(definedTypes), func(i int) string { return "type." + string(definedTypes[i].Identity.DeclID) }); err != nil {
		return err
	}
	if err := validateUniqueIDs("constants", len(a.Constants), func(i int) string { return a.Constants[i].ID }); err != nil {
		return err
	}
	if err := validateUniqueIDs("globals", len(a.Globals), func(i int) string { return a.Globals[i].ID }); err != nil {
		return err
	}
	if err := validateUniqueIDs("functions", len(a.Functions), func(i int) string { return a.Functions[i].ID }); err != nil {
		return err
	}
	if err := validateRequirements(a.Requirements); err != nil {
		return err
	}
	refs := artifactRefs{
		modulePath:         strings.TrimSpace(a.Module.Path),
		types:              collectIDs(len(definedTypes), func(i int) string { return "type." + string(definedTypes[i].Identity.DeclID) }),
		constants:          collectIDs(len(a.Constants), func(i int) string { return a.Constants[i].ID }),
		constantOrder:      a.Constants,
		untypedConstants:   make(map[string]bool, len(a.Constants)),
		globals:            collectIDs(len(a.Globals), func(i int) string { return a.Globals[i].ID }),
		globalTypes:        make(map[string]types.TypeRef, len(a.Globals)),
		functions:          collectIDs(len(a.Functions), func(i int) string { return a.Functions[i].ID }),
		functionSignatures: collectFunctionSignatures(a.Functions),
		functionPCs:        collectFunctionInstructionCounts(a.Functions),
		functionUpvalues:   collectFunctionUpvalueCounts(a.Functions),
		moduleExports:      collectModuleExports(a.Requirements),
	}
	for i, typ := range definedTypes {
		if err := validateDefinedType("type_table.nodes["+strconv.Itoa(i)+"]", typ, refs, &a.TypeTable); err != nil {
			return err
		}
	}
	constantTypes := make(map[string]types.TypeRef, len(a.Constants))
	constantUntyped := make(map[string]bool, len(a.Constants))
	for i, constant := range a.Constants {
		path := "constants[" + strconv.Itoa(i) + "]"
		if err := validateTypeRef(path+".type", constant.Type, &a.TypeTable); err != nil {
			return err
		}
		if len(constant.Value) == 0 {
			return missingValidationError(path+".value", errors.New("missing constant value"))
		}
		if !json.Valid(constant.Value) {
			return newValidationError(path+".value", errors.New("invalid json constant value"))
		}
		if err := validateConstantValue(path+".value", constant, &a.TypeTable, refs.modulePath); err != nil {
			return err
		}
		constantTypes[strings.TrimSpace(constant.ID)] = constant.Type
		constantUntyped[strings.TrimSpace(constant.ID)] = constant.Untyped
		refs.untypedConstants[strings.TrimSpace(constant.ID)] = constant.Untyped
	}
	for i, global := range a.Globals {
		refs.globalTypes[global.ID] = global.Type
		if err := validateTypeRef("globals["+strconv.Itoa(i)+"].type", global.Type, &a.TypeTable); err != nil {
			return err
		}
	}
	for i, fn := range a.Functions {
		path := "functions[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(fn.ID) == "" {
			return missingValidationError(path+".id", errors.New("missing function id"))
		}
		if err := validateFunctionSignature(path+".signature", fn.Signature, &a.TypeTable); err != nil {
			return err
		}
		if err := validateFunctionBody(path, fn, refs, &a.TypeTable); err != nil {
			return err
		}
	}
	exportNames := make(map[string]struct{}, len(a.Exports))
	for i, export := range a.Exports {
		path := "exports[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(export.Name) == "" {
			return missingValidationError(path+".name", errors.New("missing export name"))
		}
		if _, duplicate := exportNames[export.Name]; duplicate {
			return newValidationError(path+".name", fmt.Errorf("duplicate export %q", export.Name))
		}
		exportNames[export.Name] = struct{}{}
		if strings.TrimSpace(export.Kind) == "" {
			return missingValidationError(path+".kind", errors.New("missing export kind"))
		}
		if strings.TrimSpace(export.ID) == "" {
			return missingValidationError(path+".id", errors.New("missing export id"))
		}
		if export.Untyped && strings.TrimSpace(export.Kind) != "const" {
			return newValidationError(path+".untyped", errors.New("only const exports may be untyped"))
		}
		if export.Type.Valid() {
			if err := validateTypeRef(path+".type", export.Type, &a.TypeTable); err != nil {
				return err
			}
		}
		if !refs.hasExportTarget(export.Kind, export.ID) {
			if strings.TrimSpace(export.Kind) == "type" && export.Type.Valid() {
				continue
			}
			return unknownValidationError(path+".id", fmt.Errorf("unknown %s id %q", export.Kind, export.ID))
		}
		if strings.TrimSpace(export.Kind) == "const" {
			constID := strings.TrimSpace(export.ID)
			constType := constantTypes[constID]
			if !export.Type.Valid() {
				return missingValidationError(path+".type", errors.New("missing const export type"))
			}
			if !export.Type.Equal(constType) {
				return schemaMismatchValidationError(path+".type", errors.New("const export type does not match constant type"))
			}
			if export.Untyped != constantUntyped[constID] {
				return schemaMismatchValidationError(path+".untyped", errors.New("const export untyped metadata does not match constant"))
			}
		}
	}
	return nil
}

type artifactRefs struct {
	modulePath         string
	types              map[string]struct{}
	constants          map[string]struct{}
	constantOrder      []Constant
	untypedConstants   map[string]bool
	globals            map[string]struct{}
	globalTypes        map[string]types.TypeRef
	functions          map[string]struct{}
	functionSignatures map[string]types.FunctionSignature
	functionPCs        map[string]int
	functionUpvalues   map[string]int
	moduleExports      map[string]map[string]struct{}
}

func collectFunctionSignatures(functions []Function) map[string]types.FunctionSignature {
	out := make(map[string]types.FunctionSignature, len(functions))
	for _, function := range functions {
		out[strings.TrimSpace(function.ID)] = function.Signature
	}
	return out
}

func (r artifactRefs) hasExportTarget(kind, id string) bool {
	switch kind {
	case "function":
		_, ok := r.functions[id]
		return ok
	case "const":
		_, ok := r.constants[id]
		return ok
	case "global":
		_, ok := r.globals[id]
		return ok
	case "type":
		_, ok := r.types[id]
		return ok
	default:
		return false
	}
}
