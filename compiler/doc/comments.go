package doc

import (
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
)

type commentGroup struct {
	comment Comment
}

type commentIndex struct {
	document parser.Syntax
	groups   []commentGroup
}

func indexComments(document parser.Syntax) commentIndex {
	var groups []commentGroup
	for i := 0; i < len(document.Elements); i++ {
		element := document.Elements[i]
		if element.Kind != scanner.ElementLineComment && element.Kind != scanner.ElementBlockComment {
			continue
		}
		elements := []scanner.Element{element}
		if element.Kind == scanner.ElementLineComment {
			for j := i + 1; j < len(document.Elements); j++ {
				next := document.Elements[j]
				if next.Kind == scanner.ElementWhitespace || next.Kind == scanner.ElementNewline {
					continue
				}
				if next.Kind != scanner.ElementLineComment || next.Span.Start.Line != elements[len(elements)-1].Span.End.Line+1 {
					break
				}
				elements = append(elements, next)
				i = j
			}
		}
		text := cleanComments(elements)
		span := source.Span{Start: elements[0].Span.Start, End: elements[len(elements)-1].Span.End}
		groups = append(groups, commentGroup{comment: Comment{
			Key: span.Start.File + ":" + strconv.Itoa(span.Start.Offset), Text: text, Span: span,
		}})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].comment.Span.Start.Offset < groups[j].comment.Span.Start.Offset })
	return commentIndex{document: document, groups: groups}
}

func cleanComments(elements []scanner.Element) string {
	var lines []string
	for _, element := range elements {
		text := element.Lexeme
		if element.Kind == scanner.ElementLineComment {
			text = strings.TrimPrefix(text, "//")
			text = strings.TrimPrefix(text, " ")
			if isDirective(text) {
				continue
			}
		} else {
			text = strings.TrimSuffix(strings.TrimPrefix(text, "/*"), "*/")
		}
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			lines = append(lines, strings.TrimRight(line, " \t\r"))
		}
	}
	start := 0
	for start < len(lines) && lines[start] == "" {
		start++
	}
	lines = lines[start:]
	out := lines[:0]
	for _, line := range lines {
		if line == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, line)
	}
	for len(out) != 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func isDirective(text string) bool {
	if strings.HasPrefix(text, "line ") {
		return true
	}
	colon := strings.IndexByte(text, ':')
	if colon <= 0 || colon+1 >= len(text) {
		return false
	}
	for i := 0; i <= colon+1; i++ {
		if i == colon {
			continue
		}
		b := text[i]
		if !('a' <= b && b <= 'z' || '0' <= b && b <= '9') {
			return false
		}
	}
	return true
}

func (index commentIndex) leading(offset int) Comment {
	for i := len(index.groups) - 1; i >= 0; i-- {
		group := index.groups[i]
		if group.comment.Span.End.Offset > offset || group.comment.Text == "" {
			continue
		}
		between := index.document.File.Text[group.comment.Span.End.Offset:offset]
		if strings.TrimSpace(between) != "" || strings.Count(between, "\n") > 1 {
			return Comment{}
		}
		first := i
		for first > 0 {
			previous := index.groups[first-1].comment
			gap := index.document.File.Text[previous.Span.End.Offset:index.groups[first].comment.Span.Start.Offset]
			if strings.TrimSpace(gap) != "" || strings.Count(gap, "\n") > 1 {
				break
			}
			first--
		}
		if first == i {
			return group.comment
		}
		parts := make([]string, 0, i-first+1)
		for groupIndex := first; groupIndex <= i; groupIndex++ {
			if text := index.groups[groupIndex].comment.Text; text != "" {
				parts = append(parts, text)
			}
		}
		return Comment{
			Key:  index.groups[first].comment.Key,
			Text: strings.Join(parts, "\n"),
			Span: source.Span{Start: index.groups[first].comment.Span.Start, End: group.comment.Span.End},
		}
	}
	return Comment{}
}

func (index commentIndex) trailing(span source.Span) Comment {
	for _, group := range index.groups {
		if group.comment.Text == "" || group.comment.Span.Start.Offset < span.End.Offset || group.comment.Span.Start.Line != span.End.Line {
			continue
		}
		if strings.TrimSpace(index.document.File.Text[span.End.Offset:group.comment.Span.Start.Offset]) == "" {
			return group.comment
		}
		return Comment{}
	}
	return Comment{}
}

func deprecatedText(text string) string {
	for _, paragraph := range strings.Split(text, "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if strings.HasPrefix(paragraph, "Deprecated:") {
			return strings.TrimSpace(strings.TrimPrefix(paragraph, "Deprecated:"))
		}
	}
	return ""
}

func extractNotes(comment Comment) []Note {
	var out []Note
	lines := strings.Split(comment.Text, "\n")
	for i := 0; i < len(lines); {
		marker, uid, body, ok := noteStart(lines[i])
		if !ok {
			i++
			continue
		}
		bodyLines := []string{body}
		j := i + 1
		for j < len(lines) {
			if _, _, _, next := noteStart(lines[j]); next {
				break
			}
			bodyLines = append(bodyLines, lines[j])
			j++
		}
		body = strings.Join(strings.Fields(strings.Join(bodyLines, "\n")), " ")
		if body != "" {
			out = append(out, Note{Marker: marker, UID: uid, Body: body, Span: comment.Span})
		}
		i = j
	}
	return out
}

func noteStart(line string) (marker, uid, body string, ok bool) {
	line = strings.TrimSpace(line)
	open := strings.IndexByte(line, '(')
	closeParen := strings.IndexByte(line, ')')
	if open < 2 || closeParen <= open+1 {
		return "", "", "", false
	}
	marker = line[:open]
	for _, r := range marker {
		if r < 'A' || r > 'Z' {
			return "", "", "", false
		}
	}
	rest := strings.TrimSpace(line[closeParen+1:])
	if strings.HasPrefix(rest, ":") {
		rest = strings.TrimSpace(rest[1:])
	}
	return marker, line[open+1 : closeParen], rest, true
}
