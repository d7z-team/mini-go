package bytecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

// CanonicalJSON encodes an artifact already validated by its producer or
// decoder boundary.
func CanonicalJSON(a *Artifact) ([]byte, error) {
	return encodeCanonicalJSON(a)
}

func encodeCanonicalJSON(a *Artifact) ([]byte, error) {
	return encodeCanonicalValue(a)
}

// MarshalPayload encodes an instruction payload with the same string policy
// as the enclosing artifact.
func MarshalPayload(value any) (json.RawMessage, error) {
	payload, err := encodeCanonicalValue(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(payload), nil
}

func encodeCanonicalValue(value any) ([]byte, error) {
	if data, err, ok := encodeFixedJSON(value); ok {
		return data, err
	}
	var buf bytes.Buffer
	if err := encodeCanonicalValueTo(&buf, value); err != nil {
		return nil, err
	}
	return append([]byte(nil), buf.Bytes()...), nil
}

func encodeCanonicalValueTo(dst io.Writer, value any) error {
	if data, err, ok := encodeFixedJSON(value); ok {
		if err != nil {
			return err
		}
		_, err = dst.Write(data)
		return err
	}
	writer := trailingNewlineWriter{dst: dst}
	enc := json.NewEncoder(&writer)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return err
	}
	return writer.finish()
}

// trailingNewlineWriter retains the final encoded byte so Encoder's framing
// newline can be removed without buffering the complete canonical value.
type trailingNewlineWriter struct {
	dst     io.Writer
	pending byte
	hasByte bool
}

func (w *trailingNewlineWriter) Write(data []byte) (int, error) {
	length := len(data)
	if length == 0 {
		return 0, nil
	}
	if w.hasByte {
		if _, err := w.dst.Write([]byte{w.pending}); err != nil {
			return 0, err
		}
	}
	if length > 1 {
		if _, err := w.dst.Write(data[:length-1]); err != nil {
			return 0, err
		}
	}
	w.pending = data[length-1]
	w.hasByte = true
	return length, nil
}

func (w *trailingNewlineWriter) finish() error {
	if !w.hasByte || w.pending == '\n' {
		return nil
	}
	_, err := w.dst.Write([]byte{w.pending})
	return err
}

func Hash(a *Artifact) (string, error) {
	_, hash, err := EncodeJSONAndHash(a)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// HashValidated hashes an artifact that has already passed validation and has
// only had content-addressed dependency hashes rebound by the compiler.
func HashValidated(a *Artifact) (string, error) {
	payload, err := encodeCanonicalJSON(a)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func EncodeJSONAndHash(a *Artifact) ([]byte, string, error) {
	if err := ValidateArtifact(a); err != nil {
		return nil, "", err
	}
	return EncodeValidatedJSONAndHash(a)
}

// EncodeValidatedJSONAndHash encodes an artifact already validated by its
// producing or decoding boundary.
func EncodeValidatedJSONAndHash(a *Artifact) ([]byte, string, error) {
	payload, err := encodeCanonicalJSON(a)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:]), nil
}
