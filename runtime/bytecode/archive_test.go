package bytecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestPackageArchiveValidationTracksContent(t *testing.T) {
	artifact := NewArtifact("example/main", "main")
	artifact.Functions = []Function{{Code: &SlotCode{}, ID: "fn.main", Signature: testSignature("function() Void")}}
	archive, err := NewPackageArchive(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{{Name: "default", ModulePath: artifact.Module.Path, FunctionID: "fn.main"}}
	artifact.Module.Path = "changed"
	artifact.Functions[0].ID = "changed"
	if err := archive.Validate("example/main", entries); err != nil {
		t.Fatal(err)
	}
	if err := archive.Validate("changed", entries); err == nil {
		t.Fatal("accepted different module")
	}
	entries[0].FunctionID = "changed"
	if err := archive.Validate("example/main", entries); err == nil {
		t.Fatal("accepted mutated source function")
	}
	entries[0].FunctionID = "fn.main"
	image := ExecutionImage{Packages: map[string]PackageArchive{"example/main": archive}}
	encoded, err := EncodeExecutionImage(&image)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ExecutionImage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	encodedAgain, err := EncodeExecutionImage(&decoded)
	if err != nil || !bytes.Equal(encoded, encodedAgain) {
		t.Fatalf("round trip changed canonical bytes: %v", err)
	}
	if err := decoded.Packages["example/main"].Validate("example/main", entries); err != nil {
		t.Fatal(err)
	}
	clone := CloneExecutionImage(image)
	changed := clone.Packages["example/main"]
	changed.Artifact = bytes.ReplaceAll(changed.Artifact, []byte("fn.main"), []byte("fn.gone"))
	sum := sha256.Sum256(changed.Artifact)
	changed.ArtifactHash = hex.EncodeToString(sum[:])
	if err := changed.Validate("example/main", entries); err == nil {
		t.Fatal("stale proof accepted removed entry after hash recomputation")
	}
	if err := archive.Validate("example/main", entries); err != nil {
		t.Fatalf("clone mutation affected original: %v", err)
	}
	changed.Artifact[0] = '!'
	sum = sha256.Sum256(changed.Artifact)
	changed.ArtifactHash = hex.EncodeToString(sum[:])
	if err := changed.Validate("example/main", nil); err == nil {
		t.Fatal("accepted malformed content with recomputed hash")
	}
	clone.Packages["example/main"] = changed
	if _, err := EncodeExecutionImage(&clone); err == nil {
		t.Fatal("encoder trusted stale proof")
	}
	artifact.Functions[0].ID = ""
	if _, err := NewPackageArchive(&artifact); err == nil {
		t.Fatal("sealed invalid artifact")
	}
}
