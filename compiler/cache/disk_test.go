package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestResolveRootPreservesExplicitAndUsesTemporaryDefault(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "explicit")
	if got, err := ResolveDiskRoot(explicit); err != nil || got != explicit {
		t.Fatalf("ResolveDiskRoot(explicit) = %q, %v", got, err)
	}
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	want := filepath.Join(temporary, "mini-go", "cache")
	if got, err := ResolveDiskRoot(""); err != nil || got != want {
		t.Fatalf("ResolveDiskRoot(default) = %q, %v, want %q", got, err, want)
	}
}

func TestBackendRoundTripInspectVerifyAndClean(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	actionID := actionID(t, "compile")
	output := []byte("canonical output")
	outputID := OutputIDFor(output)
	entry := Entry{Output: outputID, Size: int64(len(output))}

	if _, found, err := backend.GetAction(actionID); err != nil || found {
		t.Fatalf("initial GetAction = found %v, err %v", found, err)
	}
	if err := backend.PutOutput(outputID, output); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutAction(actionID, entry); err != nil {
		t.Fatal(err)
	}
	gotEntry, found, err := backend.GetAction(actionID)
	if err != nil || !found || gotEntry != entry {
		t.Fatalf("GetAction = %#v, found %v, err %v", gotEntry, found, err)
	}
	got, found, err := backend.GetOutput(outputID)
	if err != nil || !found || string(got) != string(output) {
		t.Fatalf("GetOutput = %q, found %v, err %v", got, found, err)
	}
	stats, err := backend.Inspect()
	if err != nil || stats.Actions != 1 || stats.Objects != 1 || stats.Bytes <= int64(len(output)) {
		t.Fatalf("Inspect = %#v, %v", stats, err)
	}
	if err := backend.Verify(); err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if err := backend.Clean(); err != nil {
		t.Fatal(err)
	}
	if stats, err = backend.Inspect(); err != nil || stats != (DiskStats{}) {
		t.Fatalf("Inspect after Clean = %#v, %v", stats, err)
	}
}

func TestBackendCompressesLargeOutputs(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	output := bytes.Repeat([]byte("canonical-output\n"), 4096)
	outputID := OutputIDFor(output)
	if err := backend.PutOutput(outputID, output); err != nil {
		t.Fatal(err)
	}
	name, err := backend.cachePath(objectDirectory, outputID.String())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) >= len(output) || len(stored) <= len(objectMagic) || stored[len(objectMagic)] != objectGzip {
		t.Fatalf("compressed size = %d, source size = %d", len(stored), len(output))
	}
	decoded, found, err := backend.GetOutput(outputID)
	if err != nil || !found || !bytes.Equal(decoded, output) {
		t.Fatalf("GetOutput: found=%v err=%v", found, err)
	}
}

func TestDiskOutputEnvelopeRejectsLengthAndTrailingData(t *testing.T) {
	compressed, err := encodeDiskOutput(bytes.Repeat([]byte("compressible\n"), 4096))
	if err != nil {
		t.Fatal(err)
	}
	if compressed[len(objectMagic)] != objectGzip {
		t.Fatal("test output was not compressed")
	}
	sizeOffset := len(objectMagic) + 1
	tests := []struct {
		name string
		data []byte
	}{
		{name: "declared length too small", data: func() []byte {
			data := append([]byte(nil), compressed...)
			binary.BigEndian.PutUint64(data[sizeOffset:sizeOffset+objectSizeBytes], 1)
			return data
		}()},
		{name: "declared length too large", data: func() []byte {
			data := append([]byte(nil), compressed...)
			size := binary.BigEndian.Uint64(data[sizeOffset : sizeOffset+objectSizeBytes])
			binary.BigEndian.PutUint64(data[sizeOffset:sizeOffset+objectSizeBytes], size+1)
			return data
		}()},
		{name: "trailing payload", data: append(append([]byte(nil), compressed...), 1)},
		{name: "truncated trailer", data: compressed[:len(compressed)-1]},
		{name: "checksum", data: func() []byte {
			data := append([]byte(nil), compressed...)
			data[len(data)-8] ^= 1
			return data
		}()},
		{name: "second member", data: append(append([]byte(nil), compressed...), compressed[len(objectMagic)+1+objectSizeBytes:]...)},
		{name: "invalid magic", data: append([]byte("invalid-cache-object\x00"), objectRaw)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeDiskOutput(test.data); err == nil {
				t.Fatal("decodeDiskOutput accepted a malformed envelope")
			}
		})
	}
}

func TestBackendActionPublicationIsAtomic(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	actionID := actionID(t, "compile")
	firstData, secondData := []byte("first"), []byte("second")
	first := Entry{Output: OutputIDFor(firstData), Size: int64(len(firstData))}
	second := Entry{Output: OutputIDFor(secondData), Size: int64(len(secondData))}
	if err := backend.PutOutput(first.Output, firstData); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutOutput(second.Output, secondData); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutAction(actionID, first); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for i := 0; i < 40; i++ {
			if err := backend.PutAction(actionID, second); err != nil {
				t.Error(err)
				return
			}
			if err := backend.PutAction(actionID, first); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		entry, found, err := backend.GetAction(actionID)
		if err != nil || !found {
			t.Fatalf("GetAction = %#v, found %v, err %v", entry, found, err)
		}
		if entry != first && entry != second {
			t.Fatalf("reader observed partial action entry: %#v", entry)
		}
	}
	wait.Wait()
}

func TestBackendRejectsInvalidOutputAndEntry(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	if err := backend.PutOutput(OutputIDFor([]byte("expected")), []byte("actual")); err == nil {
		t.Fatal("PutOutput accepted mismatched content")
	}
	if err := backend.PutAction(actionID(t, "compile"), Entry{Size: -1}); err == nil {
		t.Fatal("PutAction accepted negative size")
	}
	actionID := actionID(t, "broken")
	name, err := backend.cachePath(actionDirectory, actionID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		{},
		[]byte("broken"),
		[]byte(entryVersion + " " + actionID.String() + " " + OutputIDFor(nil).String() + " 00000000000000000000\nextra"),
	} {
		if err := atomicWrite(name, data); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.GetAction(actionID); err == nil {
			t.Fatalf("GetAction accepted malformed entry %q", data)
		}
	}
}

func TestBackendMarksUseAtCoarseIntervals(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	base := time.Unix(1_700_000_000, 0)
	backend.now = func() time.Time { return base }
	data := []byte("output")
	outputID := OutputIDFor(data)
	if err := backend.PutOutput(outputID, data); err != nil {
		t.Fatal(err)
	}
	name, _ := backend.cachePath(objectDirectory, outputID.String())
	if err := os.Chtimes(name, base, base); err != nil {
		t.Fatal(err)
	}
	backend.now = func() time.Time { return base.Add(30 * time.Minute) }
	if _, _, err := backend.GetOutput(outputID); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(name)
	if !info.ModTime().Equal(base) {
		t.Fatalf("mtime changed inside coarse interval: %v", info.ModTime())
	}
	backend.now = func() time.Time { return base.Add(2 * time.Hour) }
	if _, _, err := backend.GetOutput(outputID); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(name)
	if !info.ModTime().Equal(base.Add(2 * time.Hour)) {
		t.Fatalf("mtime was not refreshed: %v", info.ModTime())
	}
}

func TestBackendTrimUsesAgeAndDailyMarker(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	base := time.Unix(1_700_000_000, 0)
	backend.now = func() time.Time { return base }
	oldAction, oldOutput := storeEntry(t, backend, "old", "old output")
	setEntryTimes(t, backend, oldAction, oldOutput, base.Add(-trimLimit-time.Hour))
	freshAction, freshOutput := storeEntry(t, backend, "fresh", "fresh output")
	setEntryTimes(t, backend, freshAction, freshOutput, base)
	orphanData := []byte("orphan output")
	orphanOutput := OutputIDFor(orphanData)
	if err := backend.PutOutput(orphanOutput, orphanData); err != nil {
		t.Fatal(err)
	}
	orphanName, _ := backend.cachePath(objectDirectory, orphanOutput.String())
	if err := os.Chtimes(orphanName, base.Add(-trimLimit-time.Hour), base.Add(-trimLimit-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := backend.Trim(); err != nil {
		t.Fatal(err)
	}
	oldActionName, _ := backend.cachePath(actionDirectory, oldAction.String())
	oldOutputName, _ := backend.cachePath(objectDirectory, oldOutput.String())
	if _, err := os.Stat(oldActionName); !os.IsNotExist(err) {
		t.Fatal("expired action survived trim")
	}
	if _, err := os.Stat(oldOutputName); !os.IsNotExist(err) {
		t.Fatal("expired output survived trim")
	}
	freshActionName, _ := backend.cachePath(actionDirectory, freshAction.String())
	freshOutputName, _ := backend.cachePath(objectDirectory, freshOutput.String())
	for _, name := range []string{freshActionName, freshOutputName} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("recent cache entry was trimmed: %v", err)
		}
	}
	if _, err := os.Stat(orphanName); !os.IsNotExist(err) {
		t.Fatal("expired orphan output survived trim")
	}

	secondAction, secondOutput := storeEntry(t, backend, "second", "second output")
	setEntryTimes(t, backend, secondAction, secondOutput, base.Add(-trimLimit-time.Hour))
	backend.now = func() time.Time { return base.Add(time.Hour) }
	if err := backend.Trim(); err != nil {
		t.Fatal(err)
	}
	secondActionName, _ := backend.cachePath(actionDirectory, secondAction.String())
	if _, err := os.Stat(secondActionName); err != nil {
		t.Fatal("daily marker did not suppress repeated trim")
	}
	backend.now = func() time.Time { return base.Add(trimInterval + time.Hour) }
	if err := backend.Trim(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secondActionName); !os.IsNotExist(err) {
		t.Fatal("expired action survived next scheduled trim")
	}
}

func TestBackendVerifyDetectsCorruptOutput(t *testing.T) {
	backend := NewDiskBackend(t.TempDir())
	_, outputID := storeEntry(t, backend, "compile", "output")
	name, _ := backend.cachePath(objectDirectory, outputID.String())
	if err := os.WriteFile(name, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backend.Verify(); err == nil {
		t.Fatal("Verify accepted corrupt output")
	}
}

func actionID(t *testing.T, name string) ActionID {
	t.Helper()
	return ActionID(sha256.Sum256([]byte(name)))
}

func storeEntry(t *testing.T, backend *DiskBackend, action, output string) (ActionID, OutputID) {
	t.Helper()
	actionID := actionID(t, action)
	data := []byte(output)
	outputID := OutputIDFor(data)
	if err := backend.PutOutput(outputID, data); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutAction(actionID, Entry{Output: outputID, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	return actionID, outputID
}

func setEntryTimes(t *testing.T, backend *DiskBackend, actionID ActionID, outputID OutputID, timestamp time.Time) {
	t.Helper()
	actionName, _ := backend.cachePath(actionDirectory, actionID.String())
	outputName, _ := backend.cachePath(objectDirectory, outputID.String())
	for _, name := range []string{actionName, outputName} {
		if err := os.Chtimes(name, timestamp, timestamp); err != nil {
			t.Fatal(err)
		}
	}
}
