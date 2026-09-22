package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

// PackageHeader contains the source identity needed to construct the import
// graph and a package cache action without building an AST.
type PackageHeader struct {
	Source      SourcePackage
	Candidates  []SourceCandidate
	Package     string
	Imports     []string
	Hash        string
	importSpans map[string]source.Span
}

func ScanPackageHeader(pkg SourcePackage) (PackageHeader, []source.Diagnostic, error) {
	return ScanPackageHeaderWithLimits(pkg, Limits{})
}

func ScanPackageHeaderWithLimits(pkg SourcePackage, limits Limits) (PackageHeader, []source.Diagnostic, error) {
	limits = normalizeLimits(limits)
	pkg, err := normalizePackage(pkg)
	if err != nil {
		return PackageHeader{}, nil, err
	}
	packageName := ""
	importSpans := map[string]source.Span{}
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	for fileIndex := range pkg.Files {
		file := source.InitializeFile(pkg.Files[fileIndex])
		pkg.Files[fileIndex] = file
		if file.OriginPath != "" {
			file.Path = file.OriginPath
		}
		scanned := scanner.ScanHeaderFileWithLimits(file, limits.parserLimits().Scanner)
		name, spans, headerDiagnostics := scanFileHeader(scanned)
		collector.AddAll(scanned.Diagnostics...)
		collector.AddAll(headerDiagnostics...)
		if packageName == "" {
			packageName = name
		} else if name != "" && name != packageName {
			collector.Add(workspaceDiagnostic("compiler.package.name.mismatch", fmt.Sprintf("file %q has package %q, want %q", scanned.File.Path, name, packageName)))
		}
		for modulePath, span := range spans {
			if _, exists := importSpans[modulePath]; !exists {
				importSpans[modulePath] = span
			}
		}
	}
	ordered := make([]string, 0, len(importSpans))
	for modulePath := range importSpans {
		ordered = append(ordered, modulePath)
	}
	sort.Strings(ordered)
	hasher := sha256.New()
	hasher.Write([]byte(source.HashFiles(pkg.Files)))
	for _, resource := range pkg.Resources {
		hasher.Write([]byte{0})
		hasher.Write([]byte(resource.Path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(resource.Hash))
	}
	return PackageHeader{
		Source: pkg, Candidates: append([]SourceCandidate(nil), pkg.SourceCandidates...), Package: packageName,
		Imports: ordered, Hash: hex.EncodeToString(hasher.Sum(nil)), importSpans: importSpans,
	}, collector.Diagnostics(), nil
}

func scanFileHeader(scanned scanner.Result) (string, map[string]source.Span, []source.Diagnostic) {
	tokens := scanned.Tokens
	if len(tokens) < 3 || tokens[0].Kind != token.Package || tokens[1].Kind != token.Ident {
		return "", nil, []source.Diagnostic{workspaceDiagnostic("parser.package", fmt.Sprintf("file %q must start with a package declaration", scanned.File.Path))}
	}
	packageName := tokens[1].Lexeme
	importSpans := map[string]source.Span{}
	var diagnostics []source.Diagnostic
	braceDepth := 0
	for index := 2; index < len(tokens); index++ {
		switch tokens[index].Kind {
		case token.Lbrace:
			braceDepth++
		case token.Rbrace:
			if braceDepth > 0 {
				braceDepth--
			}
		case token.Import:
			if braceDepth != 0 {
				continue
			}
			index++
			if index < len(tokens) && tokens[index].Kind == token.Lparen {
				for index++; index < len(tokens) && tokens[index].Kind != token.Rparen; {
					if tokens[index].Kind == token.Semicolon {
						index++
						continue
					}
					alias := ""
					if tokens[index].Kind == token.Ident || tokens[index].Kind == token.Period {
						alias = tokens[index].Lexeme
						index++
					}
					if index >= len(tokens) || tokens[index].Kind != token.String {
						diagnostics = append(diagnostics, workspaceDiagnostic("parser.import.path", fmt.Sprintf("file %q has an invalid import declaration", scanned.File.Path)))
						break
					}
					if modulePath, err := strconv.Unquote(tokens[index].Lexeme); err == nil && strings.TrimSpace(modulePath) != "" {
						if alias != "_" || modulePath != "embed" {
							if _, exists := importSpans[modulePath]; !exists {
								importSpans[modulePath] = tokens[index].Span
							}
						}
					} else {
						diagnostics = append(diagnostics, workspaceDiagnostic("parser.import.path", fmt.Sprintf("file %q has an invalid import path", scanned.File.Path)))
					}
					index++
				}
				continue
			}
			alias := ""
			if index < len(tokens) && (tokens[index].Kind == token.Ident || tokens[index].Kind == token.Period) {
				alias = tokens[index].Lexeme
				index++
			}
			if index >= len(tokens) || tokens[index].Kind != token.String {
				diagnostics = append(diagnostics, workspaceDiagnostic("parser.import.path", fmt.Sprintf("file %q has an invalid import declaration", scanned.File.Path)))
				continue
			}
			if modulePath, err := strconv.Unquote(tokens[index].Lexeme); err == nil && strings.TrimSpace(modulePath) != "" {
				if alias != "_" || modulePath != "embed" {
					if _, exists := importSpans[modulePath]; !exists {
						importSpans[modulePath] = tokens[index].Span
					}
				}
			} else {
				diagnostics = append(diagnostics, workspaceDiagnostic("parser.import.path", fmt.Sprintf("file %q has an invalid import path", scanned.File.Path)))
			}
		}
	}
	return packageName, importSpans, diagnostics
}
