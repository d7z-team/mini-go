package bytecode

import (
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler/types"
)

const (
	Format         = "mini-go-ir"
	CurrentVersion = 22
	OpcodeSet      = "minigo.ir.v10"
)

type Artifact struct {
	Format       string          `json:"format"`
	Version      int             `json:"version"`
	OpcodeSet    string          `json:"opcode_set"`
	Module       Module          `json:"module"`
	TypeTable    types.TypeTable `json:"type_table"`
	Constants    []Constant      `json:"constants,omitempty"`
	Globals      []Global        `json:"globals,omitempty"`
	Functions    []Function      `json:"functions,omitempty"`
	Exports      []Export        `json:"exports,omitempty"`
	Requirements []Requirement   `json:"requirements,omitempty"`
}

type Module struct {
	Path    string `json:"path"`
	Package string `json:"package"`
}

type Constant struct {
	ID      string          `json:"id"`
	Type    types.TypeRef   `json:"type"`
	Value   json.RawMessage `json:"value"`
	Untyped bool            `json:"untyped,omitempty"`
}

type Global struct {
	ID   string        `json:"id"`
	Type types.TypeRef `json:"type"`
}

type Function struct {
	ID            string                  `json:"id"`
	RevisionLocal bool                    `json:"revision_local,omitempty"`
	Signature     types.FunctionSignature `json:"signature"`
	Locals        []Local                 `json:"locals,omitempty"`
	ResultLocals  []string                `json:"result_locals,omitempty"`
	Upvalues      []Upvalue               `json:"upvalues,omitempty"`
	MaxStack      int                     `json:"max_stack,omitempty"`
	Instructions  []Instruction           `json:"instructions,omitempty"`
}

type Local struct {
	ID   string        `json:"id"`
	Type types.TypeRef `json:"type"`
}

type Upvalue struct {
	ID   string        `json:"id"`
	Type types.TypeRef `json:"type"`
}

type Instruction struct {
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Export struct {
	Name    string        `json:"name"`
	Kind    string        `json:"kind"`
	ID      string        `json:"id"`
	Type    types.TypeRef `json:"type,omitempty"`
	Untyped bool          `json:"untyped,omitempty"`
}

type Requirement struct {
	Kind       RequirementKind `json:"kind"`
	ModulePath string          `json:"module_path"`
	Hash       string          `json:"hash,omitempty"`
	Exports    []string        `json:"exports,omitempty"`
}

type RequirementKind string

const (
	RequirementSource RequirementKind = "source"
)

func NewArtifact(modulePath, packageName string) Artifact {
	return Artifact{
		Format:    Format,
		Version:   CurrentVersion,
		OpcodeSet: OpcodeSet,
		Module: Module{
			Path:    modulePath,
			Package: packageName,
		},
	}
}
