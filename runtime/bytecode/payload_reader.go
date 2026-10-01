package bytecode

import (
	"bytes"
	"encoding/json"
)

// Most instruction identifiers are unescaped ASCII. More general JSON strings
// retain the standard decoder's replacement and escape semantics.
func plainPayloadString(raw []byte) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	for _, b := range raw[1 : len(raw)-1] {
		if b == '\\' || b >= 0x80 {
			return "", false
		}
	}
	return string(raw[1 : len(raw)-1]), true
}

// payloadObject visits fields in source order, retaining duplicate-key merge
// semantics. Its input is a complete object already accepted by json.Valid.
type payloadObject struct {
	raw   []byte
	pos   int
	name  string
	value []byte
}

func (r *payloadObject) next() bool {
	for r.raw[r.pos] == ' ' || r.raw[r.pos] == '\t' || r.raw[r.pos] == '\n' || r.raw[r.pos] == '\r' || r.raw[r.pos] == ',' {
		r.pos++
	}
	if r.raw[r.pos] == '}' {
		return false
	}
	start := r.pos
	r.pos++
	escaped := false
	for r.raw[r.pos] != '"' {
		if r.raw[r.pos] == '\\' {
			escaped = true
			r.pos++
		} else if r.raw[r.pos] >= 0x80 {
			escaped = true
		}
		r.pos++
	}
	r.pos++
	if escaped {
		_ = json.Unmarshal(r.raw[start:r.pos], &r.name)
	} else {
		r.name = string(r.raw[start+1 : r.pos-1])
	}
	for r.raw[r.pos] == ' ' || r.raw[r.pos] == '\t' || r.raw[r.pos] == '\n' || r.raw[r.pos] == '\r' || r.raw[r.pos] == ':' {
		r.pos++
	}
	start = r.pos
	depth := 0
	quoted := false
	for r.pos < len(r.raw) {
		b := r.raw[r.pos]
		if quoted {
			switch b {
			case '\\':
				r.pos++
			case '"':
				quoted = false
			}
		} else {
			if depth == 0 && (b == ',' || b == '}') {
				break
			}
			switch b {
			case '"':
				quoted = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		r.pos++
	}
	r.value = r.raw[start:r.pos]
	return true
}
