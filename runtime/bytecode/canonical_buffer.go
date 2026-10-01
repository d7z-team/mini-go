package bytecode

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/d7z-team/mini-go/compiler/types"
)

// canonicalBuffer implements the string and raw-token policy shared by the
// generated fixed-schema encoders. It retains no state outside one encoding.
type canonicalBuffer struct {
	data         []byte
	err          error
	typeRefs     map[types.TypeRef]string
	typeRefBytes int
}

func (w *canonicalBuffer) string(value string) {
	const hex = "0123456789abcdef"
	w.data = append(w.data, '"')
	start := 0
	for i := 0; i < len(value); {
		b := value[i]
		if b >= 0x20 && b < utf8.RuneSelf && b != '\\' && b != '"' {
			i++
			continue
		}
		if b < utf8.RuneSelf {
			w.data = append(w.data, value[start:i]...)
			switch b {
			case '\\', '"':
				w.data = append(w.data, '\\', b)
			case '\n':
				w.data = append(w.data, '\\', 'n')
			case '\r':
				w.data = append(w.data, '\\', 'r')
			case '\t':
				w.data = append(w.data, '\\', 't')
			case '\b':
				w.data = append(w.data, '\\', 'b')
			case '\f':
				w.data = append(w.data, '\\', 'f')
			default:
				w.data = append(w.data, '\\', 'u', '0', '0', hex[b>>4], hex[b&15])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			w.data = append(w.data, value[start:i]...)
			w.data = append(w.data, `\ufffd`...)
			start = i + size
		} else if r == '\u2028' || r == '\u2029' {
			w.data = append(w.data, value[start:i]...)
			w.data = append(w.data, '\\', 'u', '2', '0', '2', hex[r&15])
			start = i + size
		}
		i += size
	}
	w.data = append(w.data, value[start:]...)
	w.data = append(w.data, '"')
}

func (w *canonicalBuffer) raw(value json.RawMessage) {
	if value == nil {
		w.data = append(w.data, "null"...)
		return
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, value); err != nil {
		w.err = err
		return
	}
	w.data = append(w.data, buffer.Bytes()...)
}
