package compiler

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

func (input Limits) cacheHash() string {
	limits := normalizeCompilerLimits(input)
	var material strings.Builder
	material.WriteString("mini-go/compiler-limits/v1\x00")
	for _, value := range []int{
		limits.MaxPackages, limits.MaxFiles, limits.MaxTotalSourceBytes,
		limits.MaxSourceBytes, limits.MaxTokens, limits.MaxSyntaxDepth, limits.MaxASTNodes,
		limits.MaxDiagnostics, limits.MaxSpecializations,
	} {
		material.WriteString(strconv.Itoa(value))
		material.WriteByte(0)
	}
	return source.HashText(material.String())
}

const DefaultMaxSpecializations = 100_000

// Limits bounds source-controlled compiler work. Zero values select defaults;
// Positive capacities override defaults; syntax depth retains its structural ceiling.
type Limits struct {
	MaxPackages         int
	MaxFiles            int
	MaxTotalSourceBytes int
	MaxSourceBytes      int
	MaxTokens           int
	MaxSyntaxDepth      int
	MaxASTNodes         int
	MaxDiagnostics      int
	MaxSpecializations  int
}

func boundedDiagnostics(diagnostics []source.Diagnostic, limit int) []source.Diagnostic {
	if limit <= 0 || len(diagnostics) <= limit {
		return append([]source.Diagnostic(nil), diagnostics...)
	}
	values := append([]source.Diagnostic(nil), diagnostics[:limit]...)
	return append(values, source.Diagnostic{
		Code: source.DiagnosticTruncated, Severity: source.SeverityError,
		Message: "diagnostic limit reached; additional diagnostics were omitted",
	})
}

func normalizeCompilerLimits(limits Limits) Limits {
	if limits.MaxPackages <= 0 {
		limits.MaxPackages = workspace.DefaultMaxPackages
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = workspace.DefaultMaxFiles
	}
	if limits.MaxTotalSourceBytes <= 0 {
		limits.MaxTotalSourceBytes = workspace.DefaultMaxTotalSourceBytes
	}
	if limits.MaxSourceBytes <= 0 {
		limits.MaxSourceBytes = scanner.DefaultMaxSourceBytes
	}
	if limits.MaxTokens <= 0 {
		limits.MaxTokens = scanner.DefaultMaxTokens
	}
	if limits.MaxSyntaxDepth <= 0 || limits.MaxSyntaxDepth > parser.DefaultMaxNesting {
		limits.MaxSyntaxDepth = parser.DefaultMaxNesting
	}
	if limits.MaxASTNodes <= 0 {
		limits.MaxASTNodes = ast.DefaultMaxNodes
	}
	if limits.MaxDiagnostics <= 0 {
		limits.MaxDiagnostics = workspace.DefaultMaxDiagnostics
	}
	if limits.MaxSpecializations <= 0 {
		limits.MaxSpecializations = DefaultMaxSpecializations
	}
	return limits
}

func (input Limits) workspaceLimits() workspace.Limits {
	limits := normalizeCompilerLimits(input)
	return workspace.Limits{
		MaxPackages: limits.MaxPackages, MaxFiles: limits.MaxFiles,
		MaxTotalSourceBytes: limits.MaxTotalSourceBytes, MaxSourceBytes: limits.MaxSourceBytes,
		MaxTokens: limits.MaxTokens, MaxSyntaxDepth: limits.MaxSyntaxDepth,
		MaxASTNodes: limits.MaxASTNodes, MaxDiagnostics: limits.MaxDiagnostics,
	}
}
