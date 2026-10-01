package bytecode

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Only symbol-relevant shape is retained; no decoded instructions or type
// graphs are kept alive by a sealed archive.
type packageSymbolLayout struct {
	globals   []string
	functions map[string]functionSymbolLayout
}

type functionSymbolLayout struct {
	locals, upvalues []string
	pcCount          int
}

func packageSymbolLayoutFor(artifact *Artifact, validated bool) (packageSymbolLayout, error) {
	layout := packageSymbolLayout{
		globals:   make([]string, len(artifact.Globals)),
		functions: make(map[string]functionSymbolLayout, len(artifact.Functions)),
	}
	for i, global := range artifact.Globals {
		layout.globals[i] = global.ID
	}
	for _, function := range artifact.Functions {
		if function.Code == nil {
			return packageSymbolLayout{}, errors.New("missing slot code")
		}
		shape := functionSymbolLayout{
			locals:   make([]string, len(function.Locals)),
			upvalues: make([]string, len(function.Upvalues)),
		}
		for i, local := range function.Locals {
			shape.locals[i] = local.ID
		}
		for i, upvalue := range function.Upvalues {
			shape.upvalues[i] = upvalue.ID
		}
		for _, instruction := range function.Code.Instructions {
			if !validated {
				if !IsKnownOpcode(instruction.Op) {
					return packageSymbolLayout{}, newCodedValidationError(ValidationOpcodeUnknown, "instructions", errors.New("unknown opcode"))
				}
				if _, err := function.Code.Descriptors.Payload(instruction.Op, instruction.Descriptor); err != nil {
					return packageSymbolLayout{}, err
				}
			}
			if instruction.Op != OpLabel {
				shape.pcCount++
			}
		}
		layout.functions[function.ID] = shape
	}
	return layout, nil
}

// ValidateProgramSymbols verifies a complete sidecar against one immutable
// execution image. Validation does not mutate either input.
func ValidateProgramSymbols(image *ExecutionImage, symbols *ProgramSymbols) error {
	if image == nil || symbols == nil {
		return errors.New("program and symbols are required")
	}
	if symbols.Format != SymbolsFormat || symbols.Version != SymbolsVersion || symbols.ContractID != SymbolsContract {
		return errors.New("unsupported program symbols")
	}
	if symbols.CompilerID != image.CompilerID || symbols.ProgramHash != image.Hash {
		return errors.New("program symbols identity mismatch")
	}
	imageHash, err := HashExecutionImage(*image)
	if err != nil || imageHash != image.Hash {
		return errors.New("execution image hash mismatch")
	}
	if len(symbols.Packages) != len(image.Packages) {
		return errors.New("program symbols package closure mismatch")
	}
	for modulePath, archive := range image.Packages {
		pkg, ok := symbols.Packages[modulePath]
		if !ok {
			return fmt.Errorf("program symbols missing package %q", modulePath)
		}
		if archive.validated != nil && archive.validated.hash == archive.ArtifactHash && archive.hasCanonicalProof() {
			if err := validatePackageSymbolLayout(archive.validated.module, archive.ArtifactHash, archive.validated.symbols, &pkg); err != nil {
				return fmt.Errorf("validate package symbols %q: %w", modulePath, err)
			}
			continue
		}
		artifact, err := DecodeJSON(archive.Artifact)
		if err != nil {
			return fmt.Errorf("decode package %q: %w", modulePath, err)
		}
		artifactHash, err := Hash(&artifact)
		if err != nil || artifactHash != archive.ArtifactHash {
			return fmt.Errorf("package %q code hash mismatch", modulePath)
		}
		if err := ValidatePackageSymbols(&artifact, archive.ArtifactHash, &pkg); err != nil {
			return fmt.Errorf("validate package symbols %q: %w", modulePath, err)
		}
	}
	hash, err := HashProgramSymbols(*symbols)
	if err != nil {
		return err
	}
	if symbols.Hash != hash {
		return errors.New("program symbols hash mismatch")
	}
	return nil
}

// ValidatePackageSymbols verifies source symbols against a final code artifact.
func ValidatePackageSymbols(artifact *Artifact, codeHash string, symbols *PackageSymbols) error {
	if artifact == nil || symbols == nil {
		return errors.New("artifact and package symbols are required")
	}
	layout, err := packageSymbolLayoutFor(artifact, false)
	if err != nil {
		return err
	}
	return validatePackageSymbolLayout(artifact.Module.Path, codeHash, layout, symbols)
}

func validatePackageSymbolLayout(module, codeHash string, layout packageSymbolLayout, symbols *PackageSymbols) error {
	if symbols.ModulePath != module || symbols.CodeHash != codeHash || !validSymbolHash(codeHash) {
		return errors.New("package symbols code identity mismatch")
	}
	if symbols.SourceHash != "" && !validSymbolHash(symbols.SourceHash) {
		return errors.New("package symbols source hash is invalid")
	}
	files, err := validateSymbolFiles(symbols.Files)
	if err != nil {
		return err
	}
	globals := make(map[string]struct{}, len(layout.globals))
	for _, global := range layout.globals {
		globals[global] = struct{}{}
	}
	if len(symbols.Globals) != len(globals) {
		return errors.New("global symbol count mismatch")
	}
	for index, global := range symbols.Globals {
		if _, ok := globals[global.ID]; !ok || strings.TrimSpace(global.Name) == "" {
			return fmt.Errorf("invalid global symbol %d", index)
		}
		delete(globals, global.ID)
	}
	functions := make(map[string]functionSymbolLayout, len(layout.functions))
	for id, function := range layout.functions {
		functions[id] = function
	}
	if len(symbols.Functions) != len(functions) {
		return errors.New("function symbol count mismatch")
	}
	for index := range symbols.Functions {
		function := &symbols.Functions[index]
		code, ok := functions[function.ID]
		if !ok || strings.TrimSpace(function.Name) == "" {
			return fmt.Errorf("invalid function symbol %d", index)
		}
		if err := validateFunctionSymbols(code, function, files); err != nil {
			return fmt.Errorf("function %q: %w", function.ID, err)
		}
		delete(functions, function.ID)
	}
	return nil
}

func validateSymbolFiles(input []SourceFile) (map[string]struct{}, error) {
	files := make(map[string]struct{}, len(input)*2)
	for index, file := range input {
		id, path := strings.TrimSpace(file.ID), strings.TrimSpace(file.Path)
		if id == "" || path == "" {
			return nil, fmt.Errorf("invalid source file %d", index)
		}
		if file.Hash != "" && !validSymbolHash(file.Hash) {
			return nil, fmt.Errorf("source file %d has invalid hash", index)
		}
		if _, duplicate := files[id]; duplicate {
			return nil, fmt.Errorf("duplicate source file identity %q", id)
		}
		if _, duplicate := files[path]; duplicate {
			return nil, fmt.Errorf("duplicate source file identity %q", path)
		}
		files[id], files[path] = struct{}{}, struct{}{}
	}
	return files, nil
}

func validateFunctionSymbols(code functionSymbolLayout, symbols *FunctionSymbols, files map[string]struct{}) error {
	if err := validateSymbolLocation(symbols.Declaration, files); err != nil {
		return fmt.Errorf("declaration: %w", err)
	}
	locals := make(map[string]struct{}, len(code.locals))
	for _, local := range code.locals {
		locals[local] = struct{}{}
	}
	if len(symbols.Locals) != len(locals) {
		return errors.New("local symbol count mismatch")
	}
	scopes := make(map[int]struct{}, len(symbols.Scopes))
	pcCount := code.pcCount
	for index, scope := range symbols.Scopes {
		if scope.ID <= 0 {
			return fmt.Errorf("scope %d has invalid id", index)
		}
		if _, duplicate := scopes[scope.ID]; duplicate {
			return fmt.Errorf("duplicate scope %d", scope.ID)
		}
		if scope.Parent != 0 {
			if _, ok := scopes[scope.Parent]; !ok {
				return fmt.Errorf("scope %d has unknown parent %d", scope.ID, scope.Parent)
			}
		}
		previousEnd := 0
		for _, pcRange := range scope.Ranges {
			if pcRange.Start < 0 || pcRange.Start >= pcRange.End || pcRange.End > pcCount || pcRange.Start < previousEnd {
				return fmt.Errorf("scope %d has invalid range [%d,%d)", scope.ID, pcRange.Start, pcRange.End)
			}
			previousEnd = pcRange.End
		}
		scopes[scope.ID] = struct{}{}
	}
	for index, local := range symbols.Locals {
		if _, ok := locals[local.ID]; !ok {
			return fmt.Errorf("local symbol %d references unknown id %q", index, local.ID)
		}
		if local.Scope != 0 {
			if _, ok := scopes[local.Scope]; !ok {
				return fmt.Errorf("local %q references unknown scope %d", local.ID, local.Scope)
			}
		}
		if err := validateSymbolLocation(local.Declaration, files); err != nil {
			return fmt.Errorf("local %q declaration: %w", local.ID, err)
		}
		delete(locals, local.ID)
	}
	upvalues := make(map[string]struct{}, len(code.upvalues))
	for _, upvalue := range code.upvalues {
		upvalues[upvalue] = struct{}{}
	}
	if len(symbols.Upvalues) != len(upvalues) {
		return errors.New("upvalue symbol count mismatch")
	}
	for index, upvalue := range symbols.Upvalues {
		if _, ok := upvalues[upvalue.ID]; !ok {
			return fmt.Errorf("upvalue symbol %d references unknown id %q", index, upvalue.ID)
		}
		delete(upvalues, upvalue.ID)
	}
	previousPC := -1
	for index, location := range symbols.Locations {
		if location.PC < 0 || location.PC >= pcCount || location.PC <= previousPC || len(location.Points) == 0 {
			return fmt.Errorf("invalid instruction symbol %d", index)
		}
		seen := make(map[Location]struct{}, len(location.Points))
		for _, point := range location.Points {
			if _, duplicate := seen[point]; duplicate {
				return fmt.Errorf("instruction symbol %d contains duplicate source point", index)
			}
			if err := validateSymbolLocation(&point, files); err != nil {
				return fmt.Errorf("instruction symbol %d: %w", index, err)
			}
			seen[point] = struct{}{}
		}
		previousPC = location.PC
	}
	return nil
}

func validateSymbolLocation(location *Location, files map[string]struct{}) error {
	if location == nil {
		return nil
	}
	if _, ok := files[strings.TrimSpace(location.File)]; !ok {
		return fmt.Errorf("unknown source file %q", location.File)
	}
	if location.Line <= 0 || location.Column < 0 {
		return errors.New("invalid source position")
	}
	return nil
}

func validSymbolHash(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32
}
