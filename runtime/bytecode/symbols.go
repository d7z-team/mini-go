package bytecode

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	SymbolsFormat   = "mini-go-program-symbols"
	SymbolsVersion  = 1
	SymbolsContract = "minigo.symbols.v1"
)

// ProgramSymbols contains optional source-level information for one exact
// execution image. It does not participate in program execution or identity.
type ProgramSymbols struct {
	Format       string                    `json:"format"`
	Version      int                       `json:"version"`
	CompilerID   string                    `json:"compiler_id"`
	ContractID   string                    `json:"contract_id"`
	ProgramHash  string                    `json:"program_hash"`
	Optimization uint8                     `json:"optimization"`
	Packages     map[string]PackageSymbols `json:"packages"`
	Hash         string                    `json:"hash"`
}

// EncodeProgramSymbols encodes the source sidecar using its fixed schema.
func EncodeProgramSymbols(symbols *ProgramSymbols) ([]byte, error) {
	return encodeCanonicalValue(symbols)
}

// PackageSymbols contains display names and final source mappings for one
// executable package artifact.
type PackageSymbols struct {
	ModulePath string            `json:"module_path"`
	CodeHash   string            `json:"code_hash"`
	SourceHash string            `json:"source_hash,omitempty"`
	Files      []SourceFile      `json:"files,omitempty"`
	Globals    []GlobalSymbol    `json:"globals,omitempty"`
	Functions  []FunctionSymbols `json:"functions,omitempty"`
}

type SourceFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Hash string `json:"hash,omitempty"`
}

type GlobalSymbol struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type FunctionSymbols struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Generated   bool                `json:"generated,omitempty"`
	Declaration *Location           `json:"declaration,omitempty"`
	Locals      []LocalSymbol       `json:"locals,omitempty"`
	Upvalues    []UpvalueSymbol     `json:"upvalues,omitempty"`
	Scopes      []DebugScope        `json:"scopes,omitempty"`
	Locations   []InstructionSymbol `json:"locations,omitempty"`
}

type LocalSymbol struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Scope       int       `json:"scope,omitempty"`
	Generated   bool      `json:"generated,omitempty"`
	Declaration *Location `json:"declaration,omitempty"`
}

type UpvalueSymbol struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type DebugScope struct {
	ID     int       `json:"id"`
	Parent int       `json:"parent,omitempty"`
	Ranges []PCRange `json:"ranges,omitempty"`
}

type PCRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type InstructionSymbol struct {
	PC     int        `json:"pc"`
	Points []Location `json:"points"`
}

type Location struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

func HashProgramSymbols(symbols ProgramSymbols) (string, error) {
	symbols.Hash = ""
	hasher := sha256.New()
	if err := encodeCanonicalValueTo(hasher, symbols); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func HashPackageSymbols(symbols PackageSymbols) (string, error) {
	hasher := sha256.New()
	if err := encodeCanonicalValueTo(hasher, symbols); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
