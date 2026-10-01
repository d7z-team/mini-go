package scanner

import (
	"unicode/utf8"

	"github.com/d7z-team/mini-go/compiler/token"
)

func (s *scanner) scanInterpretedString(start int) {
	s.offset++
	valid := true
	for s.offset < len(s.file.Text) {
		r, size := s.peekRune()
		if r == '\n' {
			valid = false
			s.addDiagnostic("scanner.string.newline", "newline in string literal", start, s.offset)
			break
		}
		if r == '"' {
			s.offset += size
			s.emitToken(token.String, start, s.offset)
			return
		}
		if r == '\\' {
			if !s.scanEscape(start) {
				valid = false
			}
			continue
		}
		s.offset += size
	}
	if valid {
		s.addDiagnostic("scanner.string.unterminated", "unterminated string literal", start, s.offset)
	}
	s.emitToken(token.String, start, s.offset)
}

func (s *scanner) scanRawString(start int) {
	s.offset++
	for s.offset < len(s.file.Text) {
		r, size := s.peekRune()
		s.offset += size
		if r == '`' {
			s.emitToken(token.String, start, s.offset)
			return
		}
	}
	s.addDiagnostic("scanner.raw_string.unterminated", "unterminated raw string literal", start, s.offset)
	s.emitToken(token.String, start, s.offset)
}

func (s *scanner) scanRune(start int) {
	s.offset++
	count := 0
	valid := true
	for s.offset < len(s.file.Text) {
		r, size := s.peekRune()
		if r == '\n' {
			valid = false
			s.addDiagnostic("scanner.rune.newline", "newline in rune literal", start, s.offset)
			break
		}
		if r == '\'' {
			s.offset += size
			if count != 1 {
				s.addDiagnostic("scanner.rune.count", "rune literal must contain exactly one character", start, s.offset)
			}
			s.emitToken(token.Char, start, s.offset)
			return
		}
		if r == '\\' {
			if !s.scanEscape(start) {
				valid = false
			}
			count++
			continue
		}
		s.offset += size
		count++
	}
	if valid {
		s.addDiagnostic("scanner.rune.unterminated", "unterminated rune literal", start, s.offset)
	}
	s.emitToken(token.Char, start, s.offset)
}

func (s *scanner) scanEscape(literalStart int) bool {
	start := s.offset
	s.offset++
	if s.offset >= len(s.file.Text) {
		s.addDiagnostic("scanner.escape.unterminated", "unterminated escape sequence", literalStart, s.offset)
		return false
	}
	r, size := s.peekRune()
	switch r {
	case 'a', 'b', 'f', 'n', 'r', 't', 'v', '\\', '\'', '"':
		s.offset += size
		return true
	case 'x':
		s.offset += size
		return s.scanFixedHexEscape(start, 2)
	case 'u':
		s.offset += size
		return s.scanUnicodeEscape(start, 4)
	case 'U':
		s.offset += size
		return s.scanUnicodeEscape(start, 8)
	default:
		if isOctalDigit(r) {
			return s.scanOctalEscape(start)
		}
		s.offset += size
		s.addDiagnostic("scanner.escape.invalid", "invalid escape sequence", start, s.offset)
		return false
	}
}

func (s *scanner) scanFixedHexEscape(start, digits int) bool {
	_, ok := s.scanFixedHexEscapeValue(start, digits)
	return ok
}

func (s *scanner) scanOctalEscape(start int) bool {
	value := 0
	for i := 0; i < 3; i++ {
		if s.offset >= len(s.file.Text) {
			s.addDiagnostic("scanner.escape.octal", "octal escape sequence requires three octal digits", start, s.offset)
			return false
		}
		r, size := s.peekRune()
		if !isOctalDigit(r) {
			s.addDiagnostic("scanner.escape.octal", "octal escape sequence requires three octal digits", start, s.offset+size)
			s.offset += size
			return false
		}
		value = value*8 + int(r-'0')
		s.offset += size
	}
	if value > 255 {
		s.addDiagnostic("scanner.escape.octal_range", "octal escape value must not exceed 255", start, s.offset)
		return false
	}
	return true
}

func (s *scanner) scanUnicodeEscape(start, digits int) bool {
	value, ok := s.scanFixedHexEscapeValue(start, digits)
	if !ok {
		return false
	}
	if value > utf8.MaxRune || value >= 0xD800 && value <= 0xDFFF {
		s.addDiagnostic("scanner.escape.unicode_range", "escape is not a valid Unicode code point", start, s.offset)
		return false
	}
	return true
}

func (s *scanner) scanFixedHexEscapeValue(start, digits int) (int, bool) {
	value := 0
	for i := 0; i < digits; i++ {
		if s.offset >= len(s.file.Text) {
			s.addDiagnostic("scanner.escape.short", "short hexadecimal escape sequence", start, s.offset)
			return 0, false
		}
		r, size := s.peekRune()
		if !token.IsHexDigit(r) {
			s.addDiagnostic("scanner.escape.hex", "invalid hexadecimal escape sequence", start, s.offset+size)
			s.offset += size
			return 0, false
		}
		value = value*16 + hexDigitValue(r)
		s.offset += size
	}
	return value, true
}

func hexDigitValue(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	default:
		return 0
	}
}
