package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

const (
	prepareStateFormat  = "mini-go-prepare-state"
	prepareStateVersion = 3
)

type prepareState struct {
	Format       string          `json:"format"`
	Version      int             `json:"version"`
	ActionID     string          `json:"action_id"`
	Image        json.RawMessage `json:"image"`
	TestManifest []TestEntry     `json:"test_manifest,omitempty"`
}

func (s Store) LookupPrepare(input PrepareAction) (PrepareLookup, error) {
	actionID, err := input.ID()
	if err != nil {
		return PrepareLookup{}, err
	}
	stateJSON, found, reason, err := s.load(actionID)
	if err != nil {
		return PrepareLookup{}, err
	}
	if !found {
		return PrepareLookup{Reason: reason}, nil
	}
	var state prepareState
	if err := decodeStrict(stateJSON, &state); err != nil || state.Format != prepareStateFormat || state.Version != prepareStateVersion {
		return PrepareLookup{Reason: "prepare state invalid"}, nil
	}
	if state.ActionID != actionID.String() {
		return PrepareLookup{Reason: "prepare state action mismatch"}, nil
	}
	var image ir.ExecutionImage
	if err := decodeStrict(state.Image, &image); err != nil {
		return PrepareLookup{Reason: "execution image json invalid"}, nil
	}
	if err := validatePreparedOutput(input, PreparedOutput{Image: image, TestManifest: state.TestManifest}, false); err != nil {
		return PrepareLookup{Reason: err.Error()}, nil
	}
	return PrepareLookup{Image: image, TestManifest: append([]TestEntry(nil), state.TestManifest...), Hit: true, Reason: reason}, nil
}

func (s Store) StorePrepare(input PrepareAction, output PreparedOutput) error {
	if err := validatePreparedOutput(input, output, true); err != nil {
		return err
	}
	actionID, err := input.ID()
	if err != nil {
		return err
	}
	imageJSON, err := canonicalJSON(output.Image)
	if err != nil {
		return err
	}
	stateJSON, err := canonicalJSON(prepareState{
		Format: prepareStateFormat, Version: prepareStateVersion,
		ActionID: actionID.String(), Image: imageJSON, TestManifest: append([]TestEntry(nil), output.TestManifest...),
	})
	if err != nil {
		return err
	}
	return s.store(actionID, stateJSON)
}

func validatePreparedOutput(input PrepareAction, output PreparedOutput, validateArtifacts bool) error {
	if input.Format != Format || input.Version != Version || strings.TrimSpace(input.Compiler) == "" || strings.TrimSpace(input.Contract) == "" || strings.TrimSpace(input.Mode) == "" || strings.TrimSpace(input.Root) == "" {
		return errors.New("prepare action invalid")
	}
	if err := target.Validate(output.Image.Target); err != nil {
		return fmt.Errorf("execution image target invalid: %w", err)
	}
	if output.Image.Format != ir.ExecutionFormat || output.Image.Version != ir.ExecutionVersion ||
		output.Image.CompilerID != input.Compiler || output.Image.ContractID != input.Contract ||
		output.Image.Root != input.Root || !output.Image.Target.Equal(input.Target) {
		return errors.New("execution image identity mismatch")
	}
	if !slices.Equal(output.Image.Capabilities, input.Capabilities) {
		return errors.New("execution image capabilities mismatch")
	}
	expectedEntries := append([]EntryPoint(nil), input.Entries...)
	actualEntries := append([]ir.Entry(nil), output.Image.Entries...)
	sortEntryPoints(expectedEntries)
	sortEntries(actualEntries)
	if len(expectedEntries) != len(actualEntries) {
		return errors.New("execution image entries mismatch")
	}
	for i := range expectedEntries {
		if expectedEntries[i].Name != actualEntries[i].Name || expectedEntries[i].ModulePath != actualEntries[i].ModulePath || strings.TrimSpace(actualEntries[i].FunctionID) == "" {
			return errors.New("execution image entries mismatch")
		}
	}
	if len(actualEntries) == 0 {
		return errors.New("execution image requires an entry")
	}
	if len(output.Image.Packages) != len(input.Artifacts) {
		return errors.New("execution image package closure mismatch")
	}
	seenModules := make(map[string]struct{}, len(input.Artifacts))
	for _, item := range input.Artifacts {
		decodedHash, err := hex.DecodeString(item.Hash)
		if strings.TrimSpace(item.ModulePath) == "" || err != nil || len(decodedHash) != sha256.Size {
			return errors.New("prepare action artifact invalid")
		}
		if _, exists := seenModules[item.ModulePath]; exists {
			return errors.New("prepare action contains duplicate artifact")
		}
		seenModules[item.ModulePath] = struct{}{}
		archive, ok := output.Image.Packages[item.ModulePath]
		if !ok {
			return errors.New("execution image package closure mismatch")
		}
		sum := sha256.Sum256(archive.Artifact)
		if archive.ArtifactHash != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("execution image package %q content hash mismatch", item.ModulePath)
		}
		validateEntries := false
		for _, entry := range expectedEntries {
			if entry.ModulePath == item.ModulePath {
				validateEntries = true
				break
			}
		}
		if !validateArtifacts && !validateEntries {
			continue
		}
		if err := archive.Validate(item.ModulePath, actualEntries); err != nil {
			return err
		}
	}
	imageHash, err := ir.HashExecutionImage(output.Image)
	if err != nil {
		return err
	}
	if output.Image.Hash != imageHash {
		return errors.New("execution image hash mismatch")
	}
	for i, entry := range output.TestManifest {
		if strings.TrimSpace(entry.Package) == "" || strings.TrimSpace(entry.Name) == "" || entry.Index != i {
			return errors.New("test manifest invalid")
		}
	}
	return nil
}

func sortEntries(entries []ir.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		if entries[i].ModulePath != entries[j].ModulePath {
			return entries[i].ModulePath < entries[j].ModulePath
		}
		return entries[i].FunctionID < entries[j].FunctionID
	})
}

func sortEntryPoints(entries []EntryPoint) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		if entries[i].ModulePath != entries[j].ModulePath {
			return entries[i].ModulePath < entries[j].ModulePath
		}
		return entries[i].Function < entries[j].Function
	})
}
