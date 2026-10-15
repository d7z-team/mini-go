package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/d7z-team/mini-go/rpc"
)

// ContractReference identifies one immutable MRPC source bundle.
type ContractReference struct {
	ImportPath string `json:"import_path"`
	Hash       string `json:"hash"`
}

// MRPCFile is one source file in an MRPCBundle.
type MRPCFile struct {
	Path string `json:"path"`
	Text string `json:"text"`
	Hash string `json:"hash"`
}

// MRPCBundle is the source form of one independently distributable MRPC package.
type MRPCBundle struct {
	ImportPath string     `json:"import_path"`
	Hash       string     `json:"hash"`
	Files      []MRPCFile `json:"files"`
}

// ContractResolver resolves exact, immutable MRPC source identities.
type ContractResolver interface {
	ResolveContract(context.Context, ContractReference) (MRPCBundle, error)
}

// NewMRPCBundle normalizes file metadata and computes its content identity.
func NewMRPCBundle(importPath string, files []MRPCFile) (MRPCBundle, error) {
	bundle := MRPCBundle{ImportPath: strings.TrimSpace(importPath), Files: append([]MRPCFile(nil), files...)}
	if err := normalizeMRPCBundle(&bundle, true); err != nil {
		return MRPCBundle{}, err
	}
	return bundle, nil
}

func normalizeMRPCBundle(bundle *MRPCBundle, compute bool) error {
	if bundle == nil || bundle.ImportPath == "" || strings.TrimSpace(bundle.ImportPath) != bundle.ImportPath || path.Clean(bundle.ImportPath) != bundle.ImportPath || strings.HasPrefix(bundle.ImportPath, "/") || strings.HasPrefix(bundle.ImportPath, "../") {
		return errors.New("invalid MRPC bundle import path")
	}
	if len(bundle.Files) == 0 {
		return errors.New("MRPC bundle has no files")
	}
	seen := make(map[string]struct{}, len(bundle.Files))
	for index := range bundle.Files {
		file := &bundle.Files[index]
		if file.Path == "" || strings.TrimSpace(file.Path) != file.Path || path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "/") || strings.HasPrefix(file.Path, "../") || path.Ext(file.Path) != ".mrpc" {
			return fmt.Errorf("invalid MRPC file path %q", file.Path)
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return fmt.Errorf("duplicate MRPC file path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		if !utf8.ValidString(file.Text) {
			return fmt.Errorf("MRPC file %q is not UTF-8", file.Path)
		}
		sum := sha256.Sum256([]byte(file.Text))
		hash := hex.EncodeToString(sum[:])
		if !compute && file.Hash != hash {
			return fmt.Errorf("MRPC file %q hash mismatch", file.Path)
		}
		file.Hash = hash
	}
	sort.Slice(bundle.Files, func(i, j int) bool { return bundle.Files[i].Path < bundle.Files[j].Path })
	material := struct {
		ImportPath string     `json:"import_path"`
		Files      []MRPCFile `json:"files"`
	}{ImportPath: bundle.ImportPath, Files: bundle.Files}
	encoded, err := json.Marshal(material)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte("minigo.mrpc.bundle.v1\x00"), encoded...))
	hash := hex.EncodeToString(sum[:])
	if !compute && bundle.Hash != hash {
		return errors.New("MRPC bundle hash mismatch")
	}
	bundle.Hash = hash
	return nil
}

func cloneMRPCBundle(bundle MRPCBundle) MRPCBundle {
	bundle.Files = append([]MRPCFile(nil), bundle.Files...)
	return bundle
}

// ContractRepository stores immutable bundles by exact content identity.
type ContractRepository struct {
	mu      sync.RWMutex
	bundles map[ContractReference]MRPCBundle
}

func NewContractRepository() *ContractRepository {
	return &ContractRepository{bundles: make(map[ContractReference]MRPCBundle)}
}

// Publish validates and stores all bundles atomically.
func (r *ContractRepository) Publish(bundles ...MRPCBundle) error {
	if r == nil {
		return errors.New("MRPC contract repository is nil")
	}
	seen := make(map[ContractReference]MRPCBundle, len(bundles))
	for _, bundle := range bundles {
		bundle = cloneMRPCBundle(bundle)
		if err := normalizeMRPCBundle(&bundle, false); err != nil {
			return err
		}
		ref := ContractReference{ImportPath: bundle.ImportPath, Hash: bundle.Hash}
		if previous, duplicate := seen[ref]; duplicate && !equalMRPCBundle(previous, bundle) {
			return errors.New("conflicting MRPC bundles in one publication")
		}
		seen[ref] = bundle
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bundles == nil {
		r.bundles = make(map[ContractReference]MRPCBundle)
	}
	for ref, bundle := range seen {
		if existing, ok := r.bundles[ref]; ok && !equalMRPCBundle(existing, bundle) {
			return errors.New("MRPC bundle identity conflicts with stored content")
		}
	}
	for ref, bundle := range seen {
		r.bundles[ref] = bundle
	}
	return nil
}

func (r *ContractRepository) remove(reference ContractReference) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.bundles, reference)
	r.mu.Unlock()
}

func equalMRPCBundle(left, right MRPCBundle) bool {
	if left.ImportPath != right.ImportPath || left.Hash != right.Hash || len(left.Files) != len(right.Files) {
		return false
	}
	for index := range left.Files {
		if left.Files[index] != right.Files[index] {
			return false
		}
	}
	return true
}

func (r *ContractRepository) ResolveContract(ctx context.Context, reference ContractReference) (MRPCBundle, error) {
	if r == nil {
		return MRPCBundle{}, rpc.StatusError{Code: rpc.CodeNotFound, Message: "MRPC contract repository is unavailable"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		code, message := rpc.CodeOf(err)
		return MRPCBundle{}, rpc.StatusError{Code: code, Message: message}
	}
	reference.ImportPath = strings.TrimSpace(reference.ImportPath)
	reference.Hash = strings.TrimSpace(reference.Hash)
	decodedHash, decodeErr := hex.DecodeString(reference.Hash)
	if reference.ImportPath == "" || decodeErr != nil || len(decodedHash) != sha256.Size {
		return MRPCBundle{}, rpc.StatusError{Code: rpc.CodeInvalidArgument, Message: "invalid MRPC contract reference"}
	}
	r.mu.RLock()
	bundle, ok := r.bundles[reference]
	r.mu.RUnlock()
	if !ok {
		return MRPCBundle{}, rpc.StatusError{Code: rpc.CodeNotFound, Message: "MRPC contract was not found"}
	}
	return cloneMRPCBundle(bundle), nil
}
