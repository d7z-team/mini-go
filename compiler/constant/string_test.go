package constant

import (
	"encoding/json"
	"testing"
)

func TestStringConstantOperations(t *testing.T) {
	left := String("<prefix>\n", "String", true)
	right := String("\x00\xff", "example.Label", false)
	value, ok := Binary("+", left, right)
	if !ok || value.Text != `"<prefix>\n\x00\xff"` || value.Type != "example.Label" || value.Untyped {
		t.Fatalf("concatenation: %+v %v", value, ok)
	}
	if _, ok := value.Int64(); ok {
		t.Fatal("string accepted as integer")
	}
	if _, ok := Unary("-", value); ok {
		t.Fatal("string accepted as numeric operand")
	}
	raw := json.RawMessage(`"123"`)
	str, ok := FromJSON(raw, "String", true)
	if !ok || str.Text != `"123"` || string(str.JSON()) != string(raw) {
		t.Fatalf("numeric-looking string: %+v", str)
	}
}

func TestStringConstantJSONPreservesNonUTF8Facts(t *testing.T) {
	value := String("\xff\x00\x80", "String", true)
	if got := string(value.JSON()); got != `{"bytes":"/wCA"}` {
		t.Fatalf("byte-preserving constant = %s", got)
	}
	decoded, ok := FromJSON(value.JSON(), "String", true)
	if !ok || decoded.Text != value.Text || decoded.Type != value.Type || decoded.Untyped != value.Untyped {
		t.Fatalf("constant boundary changed source fact: %+v (%v)", decoded, ok)
	}
}
