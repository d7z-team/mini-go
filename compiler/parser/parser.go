package parser

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/scanner"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/token"
)

type Result struct {
	Program     ast.Program
	Diagnostics []source.Diagnostic
	NodeCount   int
}

const (
	DefaultMaxNesting     = 512
	DefaultMaxDiagnostics = 100
)

type Limits struct {
	Scanner        scanner.Limits
	MaxNesting     int
	MaxASTNodes    int
	MaxDiagnostics int
}

func ParseSource(modulePath, path, text string) Result {
	return ParseFile(modulePath, source.NewFile("file.0", path, text))
}

func ParseFile(modulePath string, file source.File) Result {
	return ParseFileWithLimits(modulePath, file, Limits{})
}

func ParseFileWithLimits(modulePath string, file source.File, limits Limits) Result {
	limits = normalizeLimits(limits)
	return parseScanned(modulePath, scanner.ScanFileWithLimits(file, limits.Scanner), limits)
}

// ParseScanned builds the AST from one scanner result so compiler and tooling
// can share the same lossless source pass.
func ParseScanned(modulePath string, scanned scanner.Result) Result {
	return parseScanned(modulePath, scanned, normalizeLimits(Limits{}))
}

func parseScanned(modulePath string, scanned scanner.Result, limits Limits) (result Result) {
	defer func() {
		for i := range result.Diagnostics {
			result.Diagnostics[i].ModulePath = modulePath
		}
	}()
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	collector.AddAll(scanned.Diagnostics...)
	embedDirectives, embedSpans, embedDiagnostics := scanEmbedDirectives(scanned)
	collector.AddAll(embedDiagnostics...)
	p := &parser{
		modulePath: strings.TrimSpace(modulePath),
		file:       scanned.File,
		tokens:     scanned.Tokens,
		embed:      embedDirectives,
		embedSpans: embedSpans,
		limits:     limits,
	}
	program := p.parseProgram()
	for offset := range p.embed {
		p.diagnostics = append(p.diagnostics, source.Diagnostic{
			Code: "parser.embed.declaration", Severity: source.SeverityError,
			Message: "misplaced //go:embed directive; expected a package variable declaration", Primary: p.embedSpans[offset],
		})
	}
	collector.AddAll(p.diagnostics...)
	hasSyntaxErrors := source.HasErrors(collector.Diagnostics())
	structureLimits := ast.Limits{
		MaxDepth: limits.MaxNesting, MaxNodes: limits.MaxASTNodes, MaxDiagnostics: limits.MaxDiagnostics,
	}
	structureDiagnostics, structureStats := ast.ValidateStructureWithStats(&program, structureLimits)
	collector.AddAll(structureDiagnostics...)
	if source.HasErrors(structureDiagnostics) {
		return Result{Program: program, Diagnostics: collector.Diagnostics(), NodeCount: structureStats.Nodes}
	}
	if hasSyntaxErrors {
		return Result{Program: program, Diagnostics: collector.Diagnostics(), NodeCount: structureStats.Nodes}
	}
	return Result{Program: program, NodeCount: structureStats.Nodes}
}

type parser struct {
	modulePath  string
	file        source.File
	tokens      []scanner.Token
	embed       map[int][]string
	embedSpans  map[int]source.Span
	pos         int
	diagnostics []source.Diagnostic
	syncPos     int
	syncCount   int
	nestLevel   int
	noComposite int
	limits      Limits
	diagLimited bool
}

func normalizeLimits(limits Limits) Limits {
	if limits.MaxNesting <= 0 || limits.MaxNesting > DefaultMaxNesting {
		limits.MaxNesting = DefaultMaxNesting
	}
	if limits.MaxDiagnostics <= 0 || limits.MaxDiagnostics > DefaultMaxDiagnostics {
		limits.MaxDiagnostics = DefaultMaxDiagnostics
	}
	if limits.MaxASTNodes <= 0 || limits.MaxASTNodes > ast.DefaultMaxNodes {
		limits.MaxASTNodes = ast.DefaultMaxNodes
	}
	return limits
}

var stmtStart = map[token.Kind]bool{
	token.Break:       true,
	token.Const:       true,
	token.Continue:    true,
	token.Defer:       true,
	token.Fallthrough: true,
	token.For:         true,
	token.Go:          true,
	token.Goto:        true,
	token.If:          true,
	token.Return:      true,
	token.Select:      true,
	token.Switch:      true,
	token.Type:        true,
	token.Var:         true,
	token.Lbrace:      true,
}

var declStart = map[token.Kind]bool{
	token.Import: true,
	token.Const:  true,
	token.Type:   true,
	token.Var:    true,
	token.Func:   true,
}

var exprEnd = map[token.Kind]bool{
	token.Comma:     true,
	token.Colon:     true,
	token.Semicolon: true,
	token.Rparen:    true,
	token.Rbrack:    true,
	token.Rbrace:    true,
	token.EOF:       true,
}

func (p *parser) parseProgram() ast.Program {
	fileSpan, _ := p.file.Span(0, len(p.file.Text))
	program := ast.Program{
		ModulePath: p.modulePath,
		Files: []ast.File{{
			ID:   p.file.ID,
			Path: p.file.Path,
			Hash: p.file.Hash,
			Span: fileSpan,
		}},
	}
	p.expect(token.Package, "parser.package", "expected package clause")
	nameToken := p.expectIdentToken("parser.package.name", "expected package name")
	name := nameToken.Lexeme
	if name == "_" {
		p.add("parser.package.blank", "package name cannot be blank identifier", nameToken.Span)
	}
	program.Package = name
	program.PackageID = identifier(nameToken)
	program.Files[0].PackageID = program.PackageID
	p.consumeSemicolons()
	for !p.at(token.EOF) {
		before := p.pos
		p.consumeSemicolons()
		if p.at(token.EOF) {
			break
		}
		decls := p.parseDecl(true)
		program.Files[0].Decls = append(program.Files[0].Decls, decls...)
		p.consumeSemicolons()
		p.ensureProgress(before, "parser.progress.decl", declStart)
	}
	return program
}

func (p *parser) parseDecl(packageScope bool) []ast.Decl {
	switch p.current().Kind {
	case token.Import:
		return p.parseImportDecl()
	case token.Const:
		return p.parseValueDecl(ast.DeclConst, packageScope)
	case token.Var:
		return p.parseValueDecl(ast.DeclVar, packageScope)
	case token.Type:
		return p.parseTypeDecl()
	case token.Func:
		return []ast.Decl{p.parseFuncDecl()}
	default:
		invalid := p.current()
		p.errorAtCurrent("parser.decl", "expected declaration")
		p.advance()
		return []ast.Decl{{Kind: ast.DeclInvalid, Span: invalid.Span}}
	}
}

func (p *parser) parseImportDecl() []ast.Decl {
	start := p.expect(token.Import, "parser.import", "expected import")
	if p.match(token.Lparen) {
		var out []ast.Decl
		for !p.at(token.Rparen) && !p.at(token.EOF) {
			before := p.pos
			p.consumeSemicolons()
			if p.at(token.Rparen) {
				break
			}
			out = append(out, p.parseImportSpec(start))
			p.optionalSemicolon()
			p.ensureProgress(before, "parser.progress.import", declStart)
		}
		end := p.expect(token.Rparen, "parser.import.group", "expected ) after import group")
		_ = end
		return out
	}
	return []ast.Decl{p.parseImportSpec(start)}
}

func (p *parser) parseImportSpec(start scanner.Token) ast.Decl {
	alias := ""
	var aliasID ast.Identifier
	if p.at(token.Ident) || p.at(token.Period) {
		if p.peek(1).Kind == token.String {
			aliasToken := p.advance()
			alias = aliasToken.Lexeme
			aliasID = identifier(aliasToken)
		}
	}
	pathToken := p.expect(token.String, "parser.import.path", "expected import path")
	path := pathToken.Lexeme
	if unquoted, err := strconv.Unquote(pathToken.Lexeme); err == nil {
		path = unquoted
	}
	return ast.Decl{
		Kind: ast.DeclImport,
		Span: join(start, pathToken),
		Import: ast.ImportDecl{
			Path: path, PathSpan: pathToken.Span,
			Alias: alias, AliasID: aliasID,
		},
	}
}

func (p *parser) parseValueDecl(kind ast.DeclKind, packageScope bool) []ast.Decl {
	start := p.advance()
	previous := inheritedConstSpec{}
	if p.match(token.Lparen) {
		var out []ast.Decl
		constIndex := 0
		for !p.at(token.Rparen) && !p.at(token.EOF) {
			before := p.pos
			p.consumeSemicolons()
			if p.at(token.Rparen) {
				break
			}
			out = append(out, p.parseValueSpec(kind, start, constIndex, &previous, false, packageScope))
			if kind == ast.DeclConst {
				constIndex++
			}
			p.optionalSemicolon()
			p.ensureProgress(before, "parser.progress.value", declStart)
		}
		p.expect(token.Rparen, "parser.value.group", "expected ) after declaration group")
		return out
	}
	return []ast.Decl{p.parseValueSpec(kind, start, 0, &previous, true, packageScope)}
}

type inheritedConstSpec struct {
	typ    ast.TypeExpr
	values []ast.Expression
}

func (p *parser) parseValueSpec(kind ast.DeclKind, start scanner.Token, constIndex int, previous *inheritedConstSpec, declarationTarget, packageScope bool) ast.Decl {
	specStart := p.current().Span.Start.Offset
	names, nameIDs := p.parseIdentList()
	var typ ast.TypeExpr
	if !p.at(token.Assign) && !p.at(token.Semicolon) && !p.at(token.Rparen) && !p.at(token.EOF) {
		typ = p.parseType()
	}
	var values []ast.Expression
	if p.match(token.Assign) {
		values = p.parseExpressionListUntil(token.Semicolon, token.Rparen, token.EOF)
	}
	if kind == ast.DeclConst {
		if len(values) == 0 && len(previous.values) != 0 {
			values = previous.values
			if typ.Kind == ast.TypeInvalid {
				typ = previous.typ
			}
		} else if len(values) != 0 {
			previous.typ = typ
			previous.values = values
		}
		values = replaceIotaExpressions(values, constIndex)
	}
	var patterns []string
	if kind == ast.DeclVar && packageScope {
		target := specStart
		if declarationTarget {
			target = start.Span.Start.Offset
		}
		patterns = append(patterns, p.embed[target]...)
		if len(patterns) != 0 {
			delete(p.embed, target)
			delete(p.embedSpans, target)
		}
	}
	decl := ast.ValueDecl{Names: names, NameIDs: nameIDs, Type: typ, Values: values, EmbedPatterns: patterns}
	out := ast.Decl{Kind: kind, Span: startSpan(start)}
	if len(values) != 0 {
		out.Span = exprSpan(start, values[len(values)-1])
	} else if typ.Kind != ast.TypeInvalid {
		out.Span = spanJoin(startSpan(start), typ.Span)
	}
	if kind == ast.DeclConst {
		out.Const = decl
	} else {
		out.Var = decl
	}
	return out
}

func (p *parser) parseTypeDecl() []ast.Decl {
	start := p.expect(token.Type, "parser.type", "expected type")
	if p.match(token.Lparen) {
		var out []ast.Decl
		for !p.at(token.Rparen) && !p.at(token.EOF) {
			before := p.pos
			p.consumeSemicolons()
			if p.at(token.Rparen) {
				break
			}
			out = append(out, p.parseTypeSpec(start))
			p.optionalSemicolon()
			p.ensureProgress(before, "parser.progress.type", declStart)
		}
		p.expect(token.Rparen, "parser.type.group", "expected ) after type group")
		return out
	}
	return []ast.Decl{p.parseTypeSpec(start)}
}

func (p *parser) parseTypeSpec(start scanner.Token) ast.Decl {
	nameToken := p.expect(token.Ident, "parser.type.name", "expected type name")
	typeParams := p.parseTypeParams()
	alias := p.match(token.Assign)
	typ := p.parseType()
	return ast.Decl{
		Kind: ast.DeclType,
		Span: spanJoin(startSpan(start), typ.Span),
		Type: ast.TypeDecl{
			Name:       nameToken.Lexeme,
			NameID:     identifier(nameToken),
			TypeParams: typeParams,
			Type:       typ,
			Alias:      alias,
		},
	}
}

func (p *parser) parseFuncDecl() ast.Decl {
	start := p.expect(token.Func, "parser.func", "expected func")
	var receiver *ast.Field
	if p.at(token.Lparen) && p.looksLikeReceiver() {
		fields := p.parseParameterList()
		if len(fields) != 1 {
			p.add("parser.receiver.count", "method declaration requires exactly one receiver", startSpan(start))
		}
		if len(fields) != 0 {
			if fields[0].Variadic {
				p.add("parser.receiver.variadic", "method receiver cannot be variadic", fields[0].Span)
			}
			receiver = &fields[0]
		}
	}
	nameToken := p.expect(token.Ident, "parser.func.name", "expected function name")
	typeParams := p.parseTypeParams()
	params, results := p.parseSignature()
	body := ast.BlockStmt{}
	if p.at(token.Lbrace) {
		body = p.parseBlock()
	} else {
		p.errorAtCurrent("parser.func.body", "expected function body")
	}
	return ast.Decl{
		Kind: ast.DeclFunc,
		Span: spanJoin(startSpan(start), body.Span),
		Func: ast.FuncDecl{
			Name:       nameToken.Lexeme,
			NameID:     identifier(nameToken),
			Receiver:   receiver,
			TypeParams: typeParams,
			Params:     params,
			Results:    results,
			Body:       body,
		},
	}
}
