package bytecode

import (
	"encoding/json"
	"testing"
)

func TestStringConstantPreservesBytesAndRejectsAmbiguousPayloads(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`"世界"`, "世界"}, {`{"bytes":"AP+A"}`, "\x00\xff\x80"}, {`{"bytes":""}`, ""},
	} {
		got, err := DecodeStringConstant(json.RawMessage(test.raw))
		if err != nil || got != test.want {
			t.Fatalf("decode %s = %x (%v), want %x", test.raw, got, err, test.want)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"bytes":null}`, `{"bytes":1}`, `{"bytes":"AB=="}`, `{"bytes":"AA==\n"}`, `{"bytes":"!"}`, `{"bytes":"AA==","bytes":"AQ=="}`, `{"bytes":"AA==","extra":0}`, `{"bytes":"AA=="} {}`} {
		if _, err := DecodeStringConstant(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid string constant: %s", raw)
		}
	}
}

func TestStringConstantCanonicalEncoding(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "MiniGo 世界", want: `"MiniGo 世界"`},
		{name: "json escapes", value: "\x00\n\"\\<>&", want: `"\u0000\n\"\\\u003c\u003e\u0026"`},
		{name: "invalid utf8", value: string([]byte{'a', 0xff, 'b'}), want: `{"bytes":"Yf9i"}`},
		{name: "replacement rune", value: "a�b", want: `"a\ufffdb"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := string(EncodeStringConstant(test.value)); got != test.want {
				t.Fatalf("EncodeStringConstant(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}
