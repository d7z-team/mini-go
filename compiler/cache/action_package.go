package cache

import (
	"encoding/json"
	"sort"
	"strings"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

const (
	packageManifestFormat  = "mini-go-package-manifest"
	packageManifestVersion = 3
	packageStateFormat     = "mini-go-package-state"
	packageStateVersion    = 9
)

type packageState struct {
	Format     string          `json:"format"`
	Version    int             `json:"version"`
	Artifact   json.RawMessage `json:"artifact"`
	Symbols    json.RawMessage `json:"symbols"`
	ExportData json.RawMessage `json:"export_data"`
}

type packageManifestState struct {
	Format              string   `json:"format"`
	Version             int      `json:"version"`
	ArtifactHash        string   `json:"artifact_hash"`
	ExportHash          string   `json:"export_hash"`
	RuntimeDependencies []string `json:"runtime_dependencies,omitempty"`
}

func (s Store) LookupCompileManifest(input Action) (ManifestLookup, error) {
	actionID, err := packageManifestActionID(input)
	if err != nil {
		return ManifestLookup{}, err
	}
	data, found, reason, err := s.load(actionID)
	if err != nil {
		return ManifestLookup{}, err
	}
	if !found {
		return ManifestLookup{Reason: reason}, nil
	}
	var state packageManifestState
	if err := decodeStrict(data, &state); err != nil || state.Format != packageManifestFormat || state.Version != packageManifestVersion || !validHash(state.ArtifactHash) || !validHash(state.ExportHash) || !validRuntimeDependencies(state.RuntimeDependencies) {
		return ManifestLookup{Reason: "package manifest invalid"}, nil
	}
	return ManifestLookup{Manifest: Manifest{
		ArtifactHash: state.ArtifactHash, ExportHash: state.ExportHash,
		RuntimeDependencies: append([]string(nil), state.RuntimeDependencies...),
	}, Hit: true, Reason: reason}, nil
}

func (s Store) LookupCompile(input Action) (Lookup, error) {
	actionID, err := input.ID()
	if err != nil {
		return Lookup{}, err
	}
	stateJSON, found, reason, err := s.load(actionID)
	if err != nil {
		return Lookup{}, err
	}
	if !found {
		return Lookup{Reason: reason}, nil
	}
	var state packageState
	if err := decodeStrict(stateJSON, &state); err != nil || state.Format != packageStateFormat || state.Version != packageStateVersion {
		return Lookup{Reason: "package state invalid"}, nil
	}
	artifact, err := ir.DecodeJSON(state.Artifact)
	if err != nil {
		return Lookup{Reason: "artifact json invalid"}, nil
	}
	artifactHash, err := ir.HashValidated(&artifact)
	if err != nil || artifact.Module.Path != input.ModulePath || artifact.Module.Package != input.Package || !artifactRequirementsUnbound(artifact) {
		return Lookup{Reason: "artifact identity mismatch"}, nil
	}
	var symbols ir.PackageSymbols
	if err := decodeStrict(state.Symbols, &symbols); err != nil || ir.ValidatePackageSymbols(&artifact, artifactHash, &symbols) != nil {
		return Lookup{Reason: "package symbols invalid"}, nil
	}
	packageData, err := DecodeJSON(state.ExportData)
	if err != nil || packageData.ModulePath != input.ModulePath || packageData.Package != input.Package || packageData.ArtifactHash != artifactHash {
		return Lookup{Reason: "export data invalid"}, nil
	}
	return Lookup{
		Artifact: artifact, Symbols: symbols, ExportData: packageData, ArtifactHash: artifactHash,
		ExportHash: packageData.ExportHash, Hit: true, Reason: reason,
	}, nil
}

func (s Store) StoreCompile(input Action, sealed CompiledArtifact, symbols ir.PackageSymbols, packageData PackageData) (Manifest, error) {
	if err := validateCompileEntry(input, sealed, symbols, packageData); err != nil {
		return Manifest{}, err
	}
	artifact, artifactHash := sealed.artifact, sealed.hash
	actionID, err := input.ID()
	if err != nil {
		return Manifest{}, err
	}
	artifactJSON, err := ir.CanonicalJSON(&artifact)
	if err != nil {
		return Manifest{}, err
	}
	symbolJSON, err := canonicalJSON(symbols)
	if err != nil {
		return Manifest{}, err
	}
	exportJSON, err := EncodeJSON(packageData)
	if err != nil {
		return Manifest{}, err
	}
	stateJSON, err := canonicalJSON(packageState{
		Format: packageStateFormat, Version: packageStateVersion,
		Artifact: artifactJSON, Symbols: symbolJSON, ExportData: exportJSON,
	})
	if err != nil {
		return Manifest{}, err
	}
	if err := s.store(actionID, stateJSON); err != nil {
		return Manifest{}, err
	}
	manifest := NewManifest(artifact, artifactHash, packageData.ExportHash)
	if err := s.storeCompileManifest(input, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (s Store) storeCompileManifest(input Action, manifest Manifest) error {
	actionID, err := packageManifestActionID(input)
	if err != nil {
		return err
	}
	data, err := canonicalJSON(packageManifestState{
		Format: packageManifestFormat, Version: packageManifestVersion,
		ArtifactHash: manifest.ArtifactHash, ExportHash: manifest.ExportHash,
		RuntimeDependencies: append([]string(nil), manifest.RuntimeDependencies...),
	})
	if err != nil {
		return err
	}
	return s.store(actionID, data)
}

func runtimeDependencies(artifact ir.Artifact) []string {
	dependencies := make([]string, 0, len(artifact.Requirements))
	for _, requirement := range artifact.Requirements {
		modulePath := strings.TrimSpace(requirement.ModulePath)
		if requirement.Kind == ir.RequirementSource && modulePath != "" {
			dependencies = append(dependencies, modulePath)
		}
	}
	sort.Strings(dependencies)
	unique := dependencies[:0]
	for _, dependency := range dependencies {
		if len(unique) == 0 || unique[len(unique)-1] != dependency {
			unique = append(unique, dependency)
		}
	}
	return unique
}

// NewManifest projects the package action fields needed to locate a prepared image.
func NewManifest(artifact ir.Artifact, artifactHash, exportHash string) Manifest {
	return Manifest{
		ArtifactHash: artifactHash, ExportHash: exportHash,
		RuntimeDependencies: runtimeDependencies(artifact),
	}
}

func validRuntimeDependencies(dependencies []string) bool {
	for i, dependency := range dependencies {
		if strings.TrimSpace(dependency) == "" || dependency != strings.TrimSpace(dependency) || i != 0 && dependencies[i-1] >= dependency {
			return false
		}
	}
	return true
}

func packageManifestActionID(input Action) (ActionID, error) {
	packageID, err := input.ID()
	if err != nil {
		return ActionID{}, err
	}
	return hashAction(packageManifestDomain, packageID[:]), nil
}

func artifactRequirementsUnbound(artifact ir.Artifact) bool {
	for _, requirement := range artifact.Requirements {
		if requirement.Kind != ir.RequirementSource {
			continue
		}
		if strings.TrimSpace(requirement.Hash) != "" {
			return false
		}
	}
	return true
}
