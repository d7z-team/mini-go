package bytecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// archiveValidation is immutable and never serialized. Public archive bytes
// remain mutable, so every use of this proof must verify their content digest.
type archiveValidation struct {
	hash    string
	module  string
	symbols packageSymbolLayout
	bytes   int64
}

// NewPackageArchive validates and seals a canonical package artifact.
func NewPackageArchive(artifact *Artifact) (PackageArchive, error) {
	data, hash, err := EncodeJSONAndHash(artifact)
	if err != nil {
		return PackageArchive{}, err
	}
	layout, err := packageSymbolLayoutFor(artifact, true)
	if err != nil {
		return PackageArchive{}, err
	}
	proof := &archiveValidation{hash: hash, module: artifact.Module.Path, symbols: layout, bytes: 128 + int64(len(hash)+len(artifact.Module.Path))}
	for _, global := range layout.globals {
		proof.bytes += 16 + int64(len(global))
	}
	for id, function := range layout.functions {
		proof.bytes += 128 + int64(len(id))
		for _, local := range function.locals {
			proof.bytes += 16 + int64(len(local))
		}
		for _, upvalue := range function.upvalues {
			proof.bytes += 16 + int64(len(upvalue))
		}
	}
	return PackageArchive{Artifact: data, ArtifactHash: hash, validated: proof}, nil
}

func (a PackageArchive) hasCanonicalProof() bool {
	if a.validated == nil {
		return false
	}
	sum := sha256.Sum256(a.Artifact)
	return hex.EncodeToString(sum[:]) == a.validated.hash
}

// ValidationBytes estimates retained validation metadata for bounded caches.
func (a PackageArchive) ValidationBytes() int64 {
	if a.validated == nil {
		return 0
	}
	return a.validated.bytes
}

// Validate checks content identity, canonical encoding, module and entry targets.
// A content-matching private proof avoids decoding previously validated bytes.
func (a PackageArchive) Validate(module string, entries []Entry) error {
	sum := sha256.Sum256(a.Artifact)
	hash := hex.EncodeToString(sum[:])
	if hash != a.ArtifactHash {
		return fmt.Errorf("execution image package %q content hash mismatch", module)
	}
	proof := a.validated
	if proof == nil || proof.hash != hash {
		artifact, err := DecodeJSON(a.Artifact)
		if err != nil {
			return fmt.Errorf("execution image package %q invalid: %w", module, err)
		}
		encoded, err := CanonicalJSON(&artifact)
		if err != nil || !bytes.Equal(encoded, a.Artifact) {
			return fmt.Errorf("execution image package %q identity mismatch", module)
		}
		layout, err := packageSymbolLayoutFor(&artifact, true)
		if err != nil {
			return err
		}
		proof = &archiveValidation{module: artifact.Module.Path, symbols: layout}
	}
	if proof.module != module {
		return fmt.Errorf("execution image package %q invalid", module)
	}
	for _, entry := range entries {
		if entry.ModulePath != module {
			continue
		}
		if _, ok := proof.symbols.functions[entry.FunctionID]; !ok {
			return fmt.Errorf("execution image entry %q references unknown function", entry.Name)
		}
	}
	return nil
}
