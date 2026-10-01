package scanner

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/token"
)

func (s *scanner) scanNumber(start int) {
	kind := token.Int
	hexadecimal := false
	hexadecimalPoint := false
	hexadecimalExponent := false
	if s.matchAt("0x") || s.matchAt("0X") {
		hexadecimal = true
		s.offset += 2
		seenMantissaDigit := s.scanDigits(16)
		if s.peekByte() == '.' {
			kind = token.Float
			hexadecimalPoint = true
			s.offset++
			seenMantissaDigit = s.scanDigits(16) || seenMantissaDigit
		}
		if !seenMantissaDigit {
			s.addDiagnostic("scanner.number.digits", "numeric literal has no digits", s.offset, s.offset)
		}
		if s.peekByte() == 'p' || s.peekByte() == 'P' {
			kind = token.Float
			hexadecimalExponent = true
			s.offset++
			s.scanSign()
			if !s.scanDigits(10) {
				s.addDiagnostic("scanner.number.digits", "numeric literal has no digits", s.offset, s.offset)
			}
		}
	} else if s.matchAt("0b") || s.matchAt("0B") {
		s.offset += 2
		if !s.scanDigits(2) {
			s.addDiagnostic("scanner.number.digits", "numeric literal has no digits", s.offset, s.offset)
		}
	} else if s.matchAt("0o") || s.matchAt("0O") {
		s.offset += 2
		if !s.scanDigits(8) {
			s.addDiagnostic("scanner.number.digits", "numeric literal has no digits", s.offset, s.offset)
		}
	} else {
		if s.peekByte() == '.' {
			kind = token.Float
			s.offset++
			if !s.scanDigits(10) {
				s.addDiagnostic("scanner.number.digits", "numeric literal has no digits", s.offset, s.offset)
			}
		} else {
			s.scanDigits(10)
			if s.peekByte() == '.' {
				kind = token.Float
				s.offset++
				s.scanDigits(10)
			}
		}
		if s.peekByte() == 'e' || s.peekByte() == 'E' {
			kind = token.Float
			s.offset++
			s.scanSign()
			if !s.scanDigits(10) {
				s.addDiagnostic("scanner.number.digits", "numeric literal has no digits", s.offset, s.offset)
			}
		}
	}
	floating := kind == token.Float
	if s.peekByte() == 'i' {
		s.offset++
		kind = token.Imag
	}
	if hexadecimal && hexadecimalPoint && !hexadecimalExponent {
		s.addDiagnostic("scanner.number.hex_exponent", "hexadecimal floating-point literal requires a binary exponent", start, s.offset)
	}
	textEnd := s.offset
	if kind == token.Imag {
		textEnd--
	}
	text := s.file.Text[start:textEnd]
	if !hexadecimal && !floating && len(text) > 1 && text[0] == '0' && (len(text) < 2 || text[1] != 'b' && text[1] != 'B' && text[1] != 'o' && text[1] != 'O') {
		for _, digit := range text[1:] {
			if digit != '_' && (digit < '0' || digit > '7') {
				s.addDiagnostic("scanner.number.octal_digit", "legacy octal literal contains a non-octal digit", start, textEnd)
				break
			}
		}
	}
	if token.IsIdentifierStart(s.peekRuneValue()) {
		s.addDiagnostic("scanner.number.suffix", "invalid numeric literal suffix", start, s.offset)
	}
	s.validateNumberUnderscores(start, s.offset)
	s.emitToken(kind, start, s.offset)
}

func (s *scanner) validateNumberUnderscores(start, end int) {
	text := strings.TrimSuffix(s.file.Text[start:end], "i")
	for i := 0; i < len(text); i++ {
		if text[i] != '_' {
			continue
		}
		if i == 2 && len(text) >= 2 && text[0] == '0' && isBasePrefixByte(text[1]) {
			continue
		}
		if i == 0 || i+1 >= len(text) || !isNumberDigitAt(text, i-1) || !isNumberDigitAt(text, i+1) {
			s.addDiagnostic("scanner.number.underscore", "underscore in numeric literal must separate successive digits", start+i, start+i+1)
			return
		}
	}
}

func (s *scanner) scanDigits(base byte) bool {
	seenDigit := false
	for s.offset < len(s.file.Text) {
		ch := s.file.Text[s.offset]
		if ch >= '0' && ch-'0' < base && ch <= '9' || base == 16 && (ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			seenDigit = true
			s.offset++
			continue
		}
		if ch == '_' {
			s.offset++
			continue
		}
		break
	}
	return seenDigit
}

func (s *scanner) scanSign() {
	if s.peekByte() == '+' || s.peekByte() == '-' {
		s.offset++
	}
}

func isOctalDigit(r rune) bool {
	return r >= '0' && r <= '7'
}

func isBasePrefixByte(ch byte) bool {
	return ch == 'b' || ch == 'B' || ch == 'o' || ch == 'O' || ch == 'x' || ch == 'X'
}

func isNumberDigitAt(text string, index int) bool {
	if index < 0 || index >= len(text) {
		return false
	}
	ch := text[index]
	if ch >= '0' && ch <= '9' {
		return true
	}
	if !strings.HasPrefix(text, "0x") && !strings.HasPrefix(text, "0X") {
		return false
	}
	for i := 2; i < index; i++ {
		if text[i] == 'p' || text[i] == 'P' {
			return false
		}
	}
	return ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F'
}
