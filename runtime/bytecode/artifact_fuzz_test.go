package bytecode

import (
	"bytes"
	"testing"
)

func FuzzArtifactJSONRoundTrip(f *testing.F) {
	artifact := NewArtifact("fuzz/module", "fuzz")
	valid, err := EncodeJSON(&artifact)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	codeArtifact := NewArtifact("fuzz/module", "fuzz")
	codeArtifact.Functions = []Function{{
		ID: "fn.main", Signature: testSignature("function() Void"),
		Locals: []Local{{ID: "local.value", Type: testType("Int")}},
		Code:   testSlotCode([]string{}, []Instruction{{Op: OpReturn, Payload: ReturnPayload{}}}, [][2][]uint32{{nil, nil}}),
	}}
	codeJSON, err := EncodeJSON(&codeArtifact)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(codeJSON)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"format":"mini-go-ir"}`))
	f.Add([]byte{0, 1, 2, 3})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		artifact, err := DecodeJSON(data)
		if err != nil {
			return
		}
		canonical, err := EncodeJSON(&artifact)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeJSON(canonical)
		if err != nil {
			t.Fatalf("canonical artifact failed to decode: %v", err)
		}
		again, err := EncodeJSON(&decoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatal("canonical artifact encoding is not stable")
		}
	})
}
