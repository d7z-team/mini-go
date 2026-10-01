package parser

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
)

const embedDirective = "//go:embed"

func scanEmbedDirectives(scanned scanner.Document) (map[int][]string, map[int]source.Span, []source.Diagnostic) {
	if !scanned.MayContainLexeme(embedDirective) {
		return nil, nil, nil
	}
	directives := map[int][]string{}
	spans := map[int]source.Span{}
	var diagnostics []source.Diagnostic
	for index := 0; index < scanned.ElementCount(); index++ {
		if scanned.ElementKind(index) != scanner.ElementLineComment {
			continue
		}
		element := scanned.ElementRecord(index)
		if !strings.HasPrefix(element.Lexeme, embedDirective) {
			continue
		}
		if len(element.Lexeme) > len(embedDirective) && !unicode.IsSpace(rune(element.Lexeme[len(embedDirective)])) {
			continue
		}
		span := scanned.Element(index).Span
		patterns, err := parseEmbedPatterns(strings.TrimSpace(element.Lexeme[len(embedDirective):]))
		if err != nil {
			diagnostics = append(diagnostics, source.Diagnostic{
				Code: "parser.embed.directive", Severity: source.SeverityError,
				Message: err.Error(), Primary: span,
			})
			continue
		}
		target := nextDirectiveToken(scanned, index+1)
		if target < 0 {
			diagnostics = append(diagnostics, source.Diagnostic{
				Code: "parser.embed.declaration", Severity: source.SeverityError,
				Message: "//go:embed must precede a package variable declaration", Primary: span,
			})
			continue
		}
		offset := scanned.ElementRecord(target).Start
		directives[offset] = append(directives[offset], patterns...)
		if _, exists := spans[offset]; !exists {
			spans[offset] = span
		}
	}
	return directives, spans, diagnostics
}

func nextDirectiveToken(document scanner.Document, start int) int {
	for index := start; index < document.ElementCount(); index++ {
		switch document.ElementKind(index) {
		case scanner.ElementWhitespace, scanner.ElementNewline, scanner.ElementLineComment, scanner.ElementBlockComment:
			continue
		case scanner.ElementToken:
			return index
		}
	}
	return -1
}

func parseEmbedPatterns(text string) ([]string, error) {
	if text == "" {
		return nil, errors.New("//go:embed requires at least one pattern")
	}
	var patterns []string
	for len(text) != 0 {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		if text == "" {
			break
		}
		if text[0] != '`' && text[0] != '"' {
			end := strings.IndexFunc(text, unicode.IsSpace)
			if end < 0 {
				end = len(text)
			}
			patterns = append(patterns, text[:end])
			text = text[end:]
			continue
		}
		quote := text[0]
		end := 1
		for end < len(text) {
			if text[end] == quote && (quote == '`' || !escapedQuote(text, end)) {
				end++
				break
			}
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
		}
		if end > len(text) || text[end-1] != quote {
			return nil, errors.New("//go:embed contains an unterminated quoted pattern")
		}
		pattern, err := strconv.Unquote(text[:end])
		if err != nil {
			return nil, fmt.Errorf("invalid //go:embed pattern: %v", err)
		}
		patterns = append(patterns, pattern)
		text = text[end:]
	}
	for _, pattern := range patterns {
		if pattern == "" {
			return nil, errors.New("//go:embed pattern cannot be empty")
		}
	}
	return patterns, nil
}

func escapedQuote(text string, offset int) bool {
	backslashes := 0
	for offset--; offset >= 0 && text[offset] == '\\'; offset-- {
		backslashes++
	}
	return backslashes%2 != 0
}
