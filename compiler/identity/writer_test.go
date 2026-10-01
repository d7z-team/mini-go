package identity

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriterMatchesFixedWidthReferenceAcrossBuffers(t *testing.T) {
	var reference []byte
	appendUint := func(v uint64) { reference = binary.LittleEndian.AppendUint64(reference, v) }
	appendString := func(v string) { appendUint(uint64(len(v))); reference = append(reference, v...) }
	appendString("test/v1")
	w := New("test/v1")
	for _, s := range []string{"", "\x00\xff", strings.Repeat("x", 4097)} {
		w.String(s)
		appendString(s)
	}
	w.Uint(^uint64(0))
	appendUint(^uint64(0))
	w.Bool(true)
	reference = append(reference, 1)
	w.Bool(false)
	reference = append(reference, 0)
	got, err := w.Sum()
	if err != nil || got != sha256.Sum256(reference) {
		t.Fatalf("hash = %x, %v", got, err)
	}
}

func TestWriterPreservesExactJSONNumbersAndRejectsInvalidTokens(t *testing.T) {
	w := New("constant/v1")
	w.JSON(json.RawMessage(" 9007199254740993 "))
	got, err := w.Sum()
	want := New("constant/v1")
	want.String("9007199254740993")
	sum, _ := want.Sum()
	if err != nil || got != sum {
		t.Fatal("raw number changed", err)
	}
	for _, raw := range []json.RawMessage{{}, json.RawMessage("["), json.RawMessage("true false")} {
		bad := New("constant/v1")
		bad.JSON(raw)
		if _, err := bad.Sum(); err == nil {
			t.Fatalf("invalid token %q accepted", raw)
		}
	}
	nilToken, nullToken := New("constant/v1"), New("constant/v1")
	nilToken.JSON(nil)
	nullToken.JSON(json.RawMessage("null"))
	nilSum, nilErr := nilToken.Sum()
	nullSum, nullErr := nullToken.Sum()
	if nilErr != nil || nullErr != nil || nilSum != nullSum {
		t.Fatalf("nil/null semantics differ: %v, %v", nilErr, nullErr)
	}
}
