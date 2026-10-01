package cache

import (
	"errors"
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// CompiledArtifact owns a validated immutable snapshot and its content identity.
// Copies share that snapshot; callers receive mutable data only through Copy.
type CompiledArtifact struct {
	artifact ir.Artifact
	hash     string
}

// SealArtifact copies before validation so later caller mutation cannot reuse
// a proof for different code. The canonical bytes are released after hashing.
func SealArtifact(artifact ir.Artifact) (CompiledArtifact, error) {
	owned := ir.CloneArtifact(artifact)
	hash, err := ir.Hash(&owned)
	if err != nil {
		return CompiledArtifact{}, err
	}
	return CompiledArtifact{artifact: owned, hash: hash}, nil
}

func (a CompiledArtifact) Hash() string { return a.hash }

func (a CompiledArtifact) Copy() ir.Artifact { return ir.CloneArtifact(a.artifact) }

func validateCompileEntry(action Action, sealed CompiledArtifact, symbols ir.PackageSymbols, data PackageData) error {
	if sealed.hash == "" {
		return errors.New("missing validated artifact")
	}
	artifact := &sealed.artifact
	if artifact.Module.Path != action.ModulePath || artifact.Module.Package != action.Package || !artifactRequirementsUnbound(*artifact) {
		return errors.New("artifact does not match cache action")
	}
	if err := data.Validate(); err != nil {
		return fmt.Errorf("validate export data: %w", err)
	}
	if err := ir.ValidatePackageSymbols(artifact, sealed.hash, &symbols); err != nil {
		return fmt.Errorf("validate package symbols: %w", err)
	}
	if data.ModulePath != artifact.Module.Path || data.Package != artifact.Module.Package || data.ArtifactHash != sealed.hash {
		return errors.New("export data does not match artifact")
	}
	return nil
}
