package token

import "testing"

func TestIdentifierNames(t *testing.T) {
	for _, name := range []string{"_", "func", "Ω", "a9", "\U000105c0", "\u1c89", "a\U00011f50"} {
		if !IsIdentifierName(name) {
			t.Errorf("valid name %q rejected", name)
		}
	}
	for _, name := range []string{"", "9a", "a²", "Ⅳ", "a-b", "\xff", "\U00011f50"} {
		if IsIdentifierName(name) {
			t.Errorf("invalid name %q accepted", name)
		}
	}
	for _, name := range []string{"A", "Ω", "\u1c89"} {
		if !IsExportedName(name) {
			t.Errorf("exported name %q rejected", name)
		}
	}
	for _, name := range []string{"", "a", "_A", "ǅ", "\xff"} {
		if IsExportedName(name) {
			t.Errorf("unexported name %q accepted", name)
		}
	}
}
