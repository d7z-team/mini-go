// Package source defines immutable source files, spans, and diagnostics.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type File struct {
	ID   string
	Path string

	// OriginPath preserves the source location of a logically named input file.
	// When set, it is a relative source path used by diagnostics and symbols.
	OriginPath string
	Text       string
	Hash       string
	LineStarts []int
}

type FileSet struct {
	Files []File
}

type Position struct {
	File   string
	Offset int
	Line   int
	Column int
}

type Span struct {
	Start Position
	End   Position
}

type DiagnosticCode string

type DiagnosticSeverity string

const (
	SeverityError       DiagnosticSeverity = "error"
	SeverityWarning     DiagnosticSeverity = "warning"
	SeverityInformation DiagnosticSeverity = "information"
	SeverityHint        DiagnosticSeverity = "hint"
)

type Diagnostic struct {
	ModulePath string
	Code       DiagnosticCode
	Severity   DiagnosticSeverity
	Message    string
	Primary    Span
	Related    []RelatedDiagnostic
}

func NewFile(id, path, text string) File {
	return File{
		ID:         id,
		Path:       path,
		Text:       text,
		Hash:       HashText(text),
		LineStarts: lineStarts(text),
	}
}

// InitializeFile fills missing metadata derived from immutable source text.
func InitializeFile(file File) File {
	if file.Hash == "" {
		file.Hash = HashText(file.Text)
	}
	if len(file.LineStarts) == 0 {
		file.LineStarts = lineStarts(file.Text)
	}
	return file
}

// NormalizeFile refreshes metadata derived from source text.
func NormalizeFile(file File) File {
	file.Hash = HashText(file.Text)
	file.LineStarts = lineStarts(file.Text)
	return file
}

type RelatedDiagnostic struct {
	Message string
	Span    Span
}

func NewFileSet(files []File) FileSet {
	out := FileSet{Files: make([]File, len(files))}
	copy(out.Files, files)
	return out
}

func (fs *FileSet) AddFile(file File) int {
	fs.Files = append(fs.Files, file)
	return len(fs.Files) - 1
}

func (fs FileSet) FileByPath(path string) (File, bool) {
	for _, file := range fs.Files {
		if file.Path == path {
			return file, true
		}
	}
	return File{}, false
}

func (fs FileSet) FileByID(id string) (File, bool) {
	for _, file := range fs.Files {
		if file.ID == id {
			return file, true
		}
	}
	return File{}, false
}

func (f File) Position(offset int) (Position, bool) {
	if offset < 0 || offset > len(f.Text) || f.Path == "" {
		return Position{}, false
	}
	starts := f.LineStarts
	if len(starts) == 0 {
		starts = lineStarts(f.Text)
	}
	low, high := 1, len(starts)
	for low < high {
		middle := low + (high-low)/2
		if starts[middle] <= offset {
			low = middle + 1
		} else {
			high = middle
		}
	}
	lineIndex := low - 1
	filename := f.Path
	if f.OriginPath != "" {
		filename = f.OriginPath
	}
	return Position{
		File:   filename,
		Offset: offset,
		Line:   lineIndex + 1,
		Column: offset - starts[lineIndex],
	}, true
}

func lineStarts(text string) []int {
	starts := []int{0}
	for start := 0; start < len(text); {
		offset := strings.IndexByte(text[start:], '\n')
		if offset < 0 {
			break
		}
		start += offset + 1
		starts = append(starts, start)
	}
	return starts
}

func (f File) Span(start, end int) (Span, bool) {
	if start < 0 || end < start || end > len(f.Text) {
		return Span{}, false
	}
	startPos, ok := f.Position(start)
	if !ok {
		return Span{}, false
	}
	endPos, ok := f.Position(end)
	if !ok {
		return Span{}, false
	}
	return Span{Start: startPos, End: endPos}, true
}

func (p Position) Valid() bool {
	return p.File != "" && p.Offset >= 0 && p.Line > 0 && p.Column >= 0
}

func (s Span) Valid() bool {
	return s.Start.Valid() &&
		s.End.Valid() &&
		s.Start.File == s.End.File &&
		s.Start.Offset <= s.End.Offset
}

func (s Span) ContainsOffset(offset int) bool {
	return s.Valid() && offset >= s.Start.Offset && offset < s.End.Offset
}

func HashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func HashFiles(files []File) string {
	if len(files) == 0 {
		return ""
	}
	hasher := sha256.New()
	for _, file := range files {
		hash := file.Hash
		if hash == "" {
			hash = HashText(file.Text)
		}
		hasher.Write([]byte(file.ID))
		hasher.Write([]byte{0})
		hasher.Write([]byte(file.Path))
		hasher.Write([]byte{0})
		if file.OriginPath != "" {
			hasher.Write([]byte(file.OriginPath))
			hasher.Write([]byte{0})
		}
		hasher.Write([]byte(hash))
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
