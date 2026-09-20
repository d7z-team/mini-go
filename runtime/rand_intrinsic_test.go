package runtime

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"io"
	"testing"
)

type invalidEntropyReader struct {
	n   int
	err error
}

func (reader invalidEntropyReader) Read([]byte) (int, error) {
	return reader.n, reader.err
}

func TestCryptoRandReadUsesInjectedEntropy(t *testing.T) {
	machine := &vm{entropy: bytes.NewReader([]byte{1, 2, 3, 4})}
	data := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, 4), 4, 4)
	result, err := cryptoRandRead(intrinsicContext{vm: machine}, []vmValue{data})
	if err != nil {
		t.Fatal(err)
	}
	slice := data.Data.(*vmSlice)
	if got := slice.ByteBacking[:4]; !bytes.Equal(got, []byte{1, 2, 3, 4}) {
		t.Fatalf("entropy bytes = %v", got)
	}
	if n, err := asInt64(result[0]); err != nil || n != 4 || result[2].Data != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestCryptoRandReadRejectsInvalidReaderResults(t *testing.T) {
	tests := []struct {
		name   string
		reader io.Reader
	}{
		{name: "negative", reader: invalidEntropyReader{n: -1}},
		{name: "oversized", reader: invalidEntropyReader{n: 5}},
		{name: "no progress", reader: invalidEntropyReader{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			machine := &vm{entropy: test.reader}
			data := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, 4), 4, 4)
			result, err := cryptoRandRead(intrinsicContext{vm: machine}, []vmValue{data})
			if err != nil {
				t.Fatal(err)
			}
			if result[2].Data != false || result[1].Data == "" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCryptoRandReadPreservesPartialReadError(t *testing.T) {
	machine := &vm{entropy: io.MultiReader(bytes.NewReader([]byte{7, 8}), errorReader{})}
	data := newByteSliceHeaderValue("Slice<Uint8>", make([]byte, 4), 4, 4)
	first, err := cryptoRandRead(intrinsicContext{vm: machine}, []vmValue{data})
	if err != nil || first[0].materializedData() != int64(2) || first[2].Data != true {
		t.Fatalf("first result = %#v, %v", first, err)
	}
	second, err := cryptoRandRead(intrinsicContext{vm: machine}, []vmValue{data})
	if err != nil || second[0].materializedData() != int64(0) || second[2].Data != false {
		t.Fatalf("second result = %#v, %v", second, err)
	}
}

func TestInstanceEntropyDefaultsAndSurvivesPatch(t *testing.T) {
	program := patchTestProgram(t, patchGlobalArtifact(1), "entropy")
	defaultInstance, err := program.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if defaultInstance.vm.entropy != cryptorand.Reader {
		t.Fatal("default entropy source is not crypto/rand.Reader")
	}
	cleanupTestInstance(t, defaultInstance)

	entropy := bytes.NewReader([]byte{1, 2, 3, 4})
	instance, err := program.Instantiate(context.Background(), InstanceOptions{Entropy: entropy})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	plan, err := instance.PreparePatch(context.Background(), patchTestProgram(t, patchGlobalArtifact(2), "entropy-patched"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if instance.vm.entropy != entropy {
		t.Fatal("patch replaced the injected entropy source")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy failed") }
