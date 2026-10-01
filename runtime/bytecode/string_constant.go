package bytecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// DecodeStringConstant reads a Unicode JSON string or a byte-preserving
// {"bytes":"base64"} constant. The latter requires canonical base64 and exactly
// one field, keeping malformed and duplicate payloads consistent across VMs.
func DecodeStringConstant(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) != 0 && raw[0] == '"' {
		var value string
		err := json.Unmarshal(raw, &value)
		return value, err
	}
	invalid := errors.New("String constant requires JSON string or canonical base64 bytes object")
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	openingDelimiter, ok := opening.(json.Delim)
	if err != nil || !ok || openingDelimiter != json.Delim('{') {
		return "", invalid
	}
	key, err := decoder.Token()
	field, ok := key.(string)
	if err != nil || !ok || field != "bytes" {
		return "", invalid
	}
	payload, err := decoder.Token()
	encoded, ok := payload.(string)
	if err != nil || !ok {
		return "", invalid
	}
	closing, err := decoder.Token()
	closingDelimiter, ok := closing.(json.Delim)
	if err != nil || !ok || closingDelimiter != json.Delim('}') {
		return "", invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", invalid
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(value) != encoded {
		return "", invalid
	}
	return string(value), nil
}

// EncodeStringConstant preserves arbitrary bytes and emits deterministic JSON
// for Unicode strings, independent of host-specific escape choices.
func EncodeStringConstant(value string) json.RawMessage {
	if !utf8.ValidString(value) {
		return json.RawMessage(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(value)) + `"}`)
	}
	const hex = "0123456789abcdef"
	var out strings.Builder
	out.Grow(len(value) + 2)
	out.WriteByte('"')
	for offset := 0; offset < len(value); {
		r, size := utf8.DecodeRuneInString(value[offset:])
		offset += size
		switch r {
		case utf8.RuneError:
			out.WriteString(`\ufffd`)
		case '\\':
			out.WriteString(`\\`)
		case '"':
			out.WriteString(`\"`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '<':
			out.WriteString(`\u003c`)
		case '>':
			out.WriteString(`\u003e`)
		case '&':
			out.WriteString(`\u0026`)
		case '\u2028':
			out.WriteString(`\u2028`)
		case '\u2029':
			out.WriteString(`\u2029`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[byte(r)>>4])
				out.WriteByte(hex[byte(r)&0xf])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return json.RawMessage(out.String())
}
