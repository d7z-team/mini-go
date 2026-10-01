// Package identity writes deterministic, domain-separated compiler identities.
package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
)

// Writer batches a typed encoding into SHA-256 without retaining its payload.
// Each field is written in schema order; strings and sequences are length-prefixed.
type Writer struct {
	hash   hash.Hash
	buffer []byte
	err    error
	depth  int
}

// EnterStructure bounds recursive schema traversal, including pointer and slice
// cycles in caller-owned ASTs. Successful calls must pair with LeaveStructure.
// This limit exceeds the compiler's maximum accepted AST nesting depth.
func (w *Writer) EnterStructure() bool {
	if w.err != nil {
		return false
	}
	if w.depth >= 1024 {
		w.err = errors.New("compiler identity nesting limit exceeded")
		return false
	}
	w.depth++
	return true
}

// LeaveStructure finishes a successful EnterStructure without writing bytes.
func (w *Writer) LeaveStructure() {
	w.depth--
}

// New starts an identity with its versioned domain prefix.
func New(domain string) *Writer {
	w := &Writer{hash: sha256.New(), buffer: make([]byte, 0, 4096)}
	w.String(domain)
	return w
}

func (w *Writer) flush() {
	if len(w.buffer) != 0 {
		_, _ = w.hash.Write(w.buffer)
		w.buffer = w.buffer[:0]
	}
}

// Uint writes a fixed-width little-endian integer.
func (w *Writer) Uint(value uint64) {
	if len(w.buffer)+8 > cap(w.buffer) {
		w.flush()
	}
	w.buffer = binary.LittleEndian.AppendUint64(w.buffer, value)
}

// Bool writes a single zero or one byte.
func (w *Writer) Bool(value bool) {
	if len(w.buffer) == cap(w.buffer) {
		w.flush()
	}
	if value {
		w.buffer = append(w.buffer, 1)
	} else {
		w.buffer = append(w.buffer, 0)
	}
}

// String writes the byte length followed by the unchanged string bytes.
func (w *Writer) String(value string) {
	w.Uint(uint64(len(value)))
	for len(value) != 0 {
		if len(w.buffer) == cap(w.buffer) {
			w.flush()
		}
		n := min(len(value), cap(w.buffer)-len(w.buffer))
		w.buffer = append(w.buffer, value[:n]...)
		value = value[n:]
	}
}

// Bytes writes the byte length and unchanged data without copying the input.
func (w *Writer) Bytes(value []byte) {
	w.Uint(uint64(len(value)))
	for len(value) != 0 {
		if len(w.buffer) == cap(w.buffer) {
			w.flush()
		}
		n := min(len(value), cap(w.buffer)-len(w.buffer))
		w.buffer = append(w.buffer, value[:n]...)
		value = value[n:]
	}
}

// JSON preserves the canonical raw-token policy of compiler constants.
func (w *Writer) JSON(value json.RawMessage) {
	if value == nil {
		w.String("null")
		return
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		w.err = err
		return
	}
	w.String(string(bytes.TrimSuffix(data.Bytes(), []byte{'\n'})))
}

// Sum returns the accumulated digest and any encoding failure.
func (w *Writer) Sum() ([32]byte, error) {
	w.flush()
	var result [32]byte
	copy(result[:], w.hash.Sum(nil))
	return result, w.err
}
