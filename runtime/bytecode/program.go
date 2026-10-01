// Package bytecode defines Mini-Go's validated executable and debug symbol contracts.
package bytecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler/target"
)

// HashExecutionImage returns the content identity of an unsealed image.
// The Hash field itself is excluded from the identity.
func HashExecutionImage(image ExecutionImage) (string, error) {
	image.Hash = ""
	hasher := sha256.New()
	if err := encodeCanonicalValueTo(hasher, image); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

const (
	ExecutionFormat   = "mini-go-execution-image"
	ExecutionVersion  = 18
	ExecutionContract = "minigo.execution.v19"
	DefaultEntryName  = "default"
)

// PackageArchive contains the validated runtime artifact for one package.
type PackageArchive struct {
	Artifact     json.RawMessage `json:"artifact"`
	ArtifactHash string          `json:"artifact_hash"`
	validated    *archiveValidation
}

type Entry struct {
	Name       string `json:"name"`
	ModulePath string `json:"module_path"`
	FunctionID string `json:"function_id"`
}

// ExecutionImage is a sealed package graph with explicit callable entries.
type ExecutionImage struct {
	Format       string                    `json:"format"`
	Version      int                       `json:"version"`
	CompilerID   string                    `json:"compiler_id"`
	ContractID   string                    `json:"contract_id"`
	Target       target.Target             `json:"target"`
	Root         string                    `json:"root"`
	Entries      []Entry                   `json:"entries"`
	Capabilities []string                  `json:"capabilities,omitempty"`
	Packages     map[string]PackageArchive `json:"packages"`
	Hash         string                    `json:"hash"`
}

// EncodeExecutionImage encodes a prepared image with canonical field, map-key
// and string ordering. Package payloads must remain valid JSON.
func EncodeExecutionImage(image *ExecutionImage) ([]byte, error) {
	return encodeCanonicalValue(image)
}

func (image ExecutionImage) DefaultEntry() (Entry, bool) {
	for _, entry := range image.Entries {
		if entry.Name == DefaultEntryName {
			return entry, true
		}
	}
	return Entry{}, false
}
