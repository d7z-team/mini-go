// Package cache defines compiler action storage and cross-package export data.
package cache

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/identity"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

const (
	ExportFormat  = "mini-go-package-export"
	ExportVersion = 11
)

type PackageData struct {
	Format           string            `json:"format"`
	Version          int               `json:"version"`
	ModulePath       string            `json:"module_path"`
	Package          string            `json:"package"`
	ArtifactHash     string            `json:"artifact_hash"`
	ExportHash       string            `json:"export_hash"`
	TypeTable        types.TypeTable   `json:"type_table"`
	Constants        []ir.Constant     `json:"constants,omitempty"`
	Exports          []ir.Export       `json:"exports,omitempty"`
	Requirements     []ir.Requirement  `json:"requirements,omitempty"`
	SourceFiles      []ir.SourceFile   `json:"source_files,omitempty"`
	GenericTemplates []GenericTemplate `json:"generic_templates,omitempty"`
}

type GenericTemplate struct {
	DeclID     types.DeclID       `json:"decl_id"`
	Kind       string             `json:"kind"`
	Name       string             `json:"name"`
	Type       string             `json:"type,omitempty"`
	Decl       ast.Decl           `json:"decl"`
	References []GenericReference `json:"references,omitempty"`
}

type GenericReference struct {
	Node ast.NodeID  `json:"node"`
	Name string      `json:"name"`
	Span source.Span `json:"span"`
}

func FromArtifact(artifact ir.Artifact) (PackageData, error) {
	if err := ir.ValidateArtifact(&artifact); err != nil {
		return PackageData{}, err
	}
	artifactHash, err := ir.HashValidated(&artifact)
	if err != nil {
		return PackageData{}, err
	}
	data := PackageData{
		Format: ExportFormat, Version: ExportVersion,
		ModulePath:   strings.TrimSpace(artifact.Module.Path),
		Package:      strings.TrimSpace(artifact.Module.Package),
		ArtifactHash: artifactHash,
		TypeTable:    artifact.TypeTable,
		Constants:    append([]ir.Constant(nil), artifact.Constants...),
		Exports:      append([]ir.Export(nil), artifact.Exports...),
		Requirements: append([]ir.Requirement(nil), artifact.Requirements...),
	}
	data.ExportHash, err = data.Hash()
	return data, err
}

// WithArtifact seals compiler export data against the final runtime artifact.
// Compiler-only type nodes and generic templates remain in export data, while
// every artifact-derived field is replaced from the validated artifact.
func (p PackageData) WithArtifact(artifact ir.Artifact) (PackageData, error) {
	if err := ir.ValidateArtifact(&artifact); err != nil {
		return PackageData{}, err
	}
	modulePath := strings.TrimSpace(artifact.Module.Path)
	packageName := strings.TrimSpace(artifact.Module.Package)
	if p.ModulePath != modulePath || p.Package != packageName {
		return PackageData{}, fmt.Errorf("artifact module %s/%s does not match export data %s/%s", modulePath, packageName, p.ModulePath, p.Package)
	}
	artifactHash, err := ir.HashValidated(&artifact)
	if err != nil {
		return PackageData{}, err
	}
	p.ArtifactHash = artifactHash
	p.Constants = append([]ir.Constant(nil), artifact.Constants...)
	p.Exports = append([]ir.Export(nil), artifact.Exports...)
	p.Requirements = append([]ir.Requirement(nil), artifact.Requirements...)
	p.ExportHash, err = p.Hash()
	if err != nil {
		return PackageData{}, err
	}
	if err := p.Validate(); err != nil {
		return PackageData{}, err
	}
	return p, nil
}

// BindArtifact records dependency hashes finalized after a package cache hit.
// The package's exported API is unchanged, so its existing export hash remains valid.
func (p PackageData) BindArtifact(artifact ir.Artifact, artifactHash string) (PackageData, error) {
	modulePath := strings.TrimSpace(artifact.Module.Path)
	packageName := strings.TrimSpace(artifact.Module.Package)
	if p.ModulePath != modulePath || p.Package != packageName {
		return PackageData{}, fmt.Errorf("artifact module %s/%s does not match export data %s/%s", modulePath, packageName, p.ModulePath, p.Package)
	}
	if !validHash(artifactHash) {
		return PackageData{}, errors.New("artifact contains an invalid hash")
	}
	p.ArtifactHash = artifactHash
	p.Requirements = append([]ir.Requirement(nil), artifact.Requirements...)
	return p, nil
}

func (p PackageData) ArtifactView() ir.Artifact {
	return ir.Artifact{
		Format: ir.Format, Version: ir.CurrentVersion, OpcodeSet: ir.OpcodeSet,
		Module:       ir.Module{Path: strings.TrimSpace(p.ModulePath), Package: strings.TrimSpace(p.Package)},
		TypeTable:    p.TypeTable,
		Constants:    p.Constants,
		Exports:      p.Exports,
		Requirements: p.Requirements,
	}
}

func (p PackageData) Validate() error {
	if p.Format != ExportFormat || p.Version != ExportVersion {
		return fmt.Errorf("unsupported export data %q version %d", p.Format, p.Version)
	}
	if strings.TrimSpace(p.ModulePath) == "" || strings.TrimSpace(p.Package) == "" {
		return errors.New("export data requires module path and package")
	}
	if !validHash(p.ArtifactHash) || !validHash(p.ExportHash) {
		return errors.New("export data contains an invalid hash")
	}
	if err := p.TypeTable.ValidateCompiler(); err != nil {
		return fmt.Errorf("validate export type table: %w", err)
	}
	hash, err := p.Hash()
	if err != nil {
		return err
	}
	if hash != p.ExportHash {
		return errors.New("export data hash mismatch")
	}
	seen := make(map[types.DeclID]struct{}, len(p.GenericTemplates))
	for _, template := range p.GenericTemplates {
		if template.DeclID == "" || strings.TrimSpace(template.Name) == "" {
			return errors.New("invalid generic template declaration")
		}
		if _, exists := seen[template.DeclID]; exists {
			return fmt.Errorf("duplicate generic template %q", template.DeclID)
		}
		seen[template.DeclID] = struct{}{}
		if template.Decl.NodeID == 0 || template.Decl.Kind == ast.DeclInvalid {
			return fmt.Errorf("generic template %q has no declaration", template.DeclID)
		}
		for _, reference := range template.References {
			if reference.Node == 0 || strings.TrimSpace(reference.Name) == "" {
				return fmt.Errorf("generic template %q has an invalid package reference", template.DeclID)
			}
		}
		switch template.Kind {
		case "type":
			if template.Decl.Kind != ast.DeclType || len(template.Decl.Type.TypeParams) == 0 {
				return fmt.Errorf("generic template %q has an invalid type declaration", template.DeclID)
			}
		case "function":
			if template.Decl.Kind != ast.DeclFunc || len(template.Decl.Func.TypeParams) == 0 || template.Decl.Func.Receiver != nil {
				return fmt.Errorf("generic template %q has an invalid function declaration", template.DeclID)
			}
		case "method":
			if template.Decl.Kind != ast.DeclFunc || template.Decl.Func.Receiver == nil {
				return fmt.Errorf("generic template %q has an invalid method declaration", template.DeclID)
			}
		default:
			return fmt.Errorf("generic template %q has invalid kind %q", template.DeclID, template.Kind)
		}
	}
	return nil
}

func (p PackageData) Hash() (string, error) {
	data := p
	data.ArtifactHash = ""
	data.ExportHash = ""
	data.SourceFiles = nil
	data.Requirements = nil
	exportedConstants := map[string]struct{}{}
	for _, exported := range data.Exports {
		if exported.Kind == "const" {
			exportedConstants[strings.TrimSpace(exported.ID)] = struct{}{}
		}
	}
	constants := make([]ir.Constant, 0, len(exportedConstants))
	for _, constant := range data.Constants {
		if _, ok := exportedConstants[strings.TrimSpace(constant.ID)]; ok {
			constants = append(constants, constant)
		}
	}
	data.Constants = constants
	w := identity.New("mini-go/export/binary/v5")
	EncodeIdentityPackageData(w, data)
	sum, err := w.Sum()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

func EncodeJSON(data PackageData) ([]byte, error) {
	if err := data.Validate(); err != nil {
		return nil, err
	}
	return encode(data)
}

func DecodeJSON(data []byte) (PackageData, error) {
	var out PackageData
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return PackageData{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return PackageData{}, errors.New("unexpected trailing JSON value")
		}
		return PackageData{}, err
	}
	if err := out.Validate(); err != nil {
		return PackageData{}, err
	}
	return out, nil
}

func encode(data PackageData) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(data); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func validHash(hash string) bool {
	hash = strings.TrimSpace(hash)
	if len(hash) != 64 {
		return false
	}
	for i := range hash {
		if hash[i] < '0' || hash[i] > '9' && hash[i] < 'a' || hash[i] > 'f' {
			return false
		}
	}
	return true
}
