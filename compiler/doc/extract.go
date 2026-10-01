package doc

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/d7z-team/mini-go/compiler/analysis"
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	tokenpkg "github.com/d7z-team/mini-go/compiler/token"
	"github.com/d7z-team/mini-go/compiler/types"
)

func Build(request BuildRequest) (Catalog, error) {
	selected := make(map[string]bool, len(request.Packages))
	for _, modulePath := range request.Packages {
		modulePath = strings.TrimSpace(modulePath)
		if modulePath == "" {
			return Catalog{}, errors.New("empty documentation package path")
		}
		if _, ok := request.Workspace.Packages[modulePath]; !ok {
			return Catalog{}, fmt.Errorf("documentation package %q was not checked", modulePath)
		}
		selected[modulePath] = true
	}
	catalog := Catalog{Target: request.Workspace.Target, Diagnostics: append([]source.Diagnostic(nil), request.Workspace.Diagnostics...)}
	paths := make([]string, 0, len(request.Workspace.Packages))
	for modulePath := range request.Workspace.Packages {
		paths = append(paths, modulePath)
	}
	sort.Strings(paths)
	for _, modulePath := range paths {
		pkg, err := extractPackage(modulePath, request.Workspace.Packages[modulePath], selected[modulePath])
		if err != nil {
			return Catalog{}, err
		}
		catalog.Packages = append(catalog.Packages, pkg)
	}
	return catalog, nil
}

func extractPackage(modulePath string, input analysis.Package, generate bool) (Package, error) {
	info := input.Checked.Info
	if info == nil {
		return Package{}, fmt.Errorf("package %q has no semantic information", modulePath)
	}
	pkg := Package{ModulePath: modulePath, Name: info.Package, Generate: generate, Imports: map[string]string{}}
	documents := make(map[string]parser.Syntax, len(input.Documents))
	comments := make(map[string]commentIndex, len(input.Documents))
	for _, inputDocument := range input.Documents {
		document := inputDocument.Syntax()
		documents[document.File.Path] = document
		comments[document.File.Path] = indexComments(document)
		packageOffset := document.Program.PackageID.Span.Start.Offset
		if len(document.Tokens) != 0 {
			packageOffset = document.Tokens[0].Span.Start.Offset
		}
		if packageDoc := comments[document.File.Path].leading(packageOffset); packageDoc.Text != "" {
			if pkg.Doc.Text != "" {
				pkg.Doc.Text += "\n\n"
			}
			if pkg.Doc.Key == "" {
				pkg.Doc = packageDoc
			} else {
				pkg.Doc.Text += packageDoc.Text
			}
		}
	}
	for _, file := range input.Checked.Program.Files {
		if strings.HasSuffix(file.Path, ".mrpc") {
			continue
		}
		document, ok := documents[file.Path]
		if !ok {
			return Package{}, fmt.Errorf("package %q is missing lossless document %q", modulePath, file.Path)
		}
		index := comments[file.Path]
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclImport {
				alias := decl.Import.Alias
				if alias == "" {
					alias = defaultImportName(decl.Import.Path)
				}
				if alias != "_" && alias != "." {
					pkg.Imports[alias] = decl.Import.Path
				}
				continue
			}
			symbols := extractDeclaration(modulePath, decl, document, index, info)
			pkg.Symbols = append(pkg.Symbols, symbols...)
		}
	}
	attachMethods(&pkg)
	for _, document := range input.Documents {
		index := comments[document.File.Path]
		for _, group := range index.groups {
			pkg.Notes = append(pkg.Notes, extractNotes(group.comment)...)
		}
	}
	sort.SliceStable(pkg.Notes, func(i, j int) bool {
		left, right := pkg.Notes[i], pkg.Notes[j]
		if left.Span.Start.File != right.Span.Start.File {
			return left.Span.Start.File < right.Span.Start.File
		}
		if left.Span.Start.Offset != right.Span.Start.Offset {
			return left.Span.Start.Offset < right.Span.Start.Offset
		}
		if left.Marker != right.Marker {
			return left.Marker < right.Marker
		}
		return left.UID < right.UID
	})
	sort.SliceStable(pkg.Symbols, func(i, j int) bool {
		if pkg.Symbols[i].Name != pkg.Symbols[j].Name {
			return pkg.Symbols[i].Name < pkg.Symbols[j].Name
		}
		return pkg.Symbols[i].Kind < pkg.Symbols[j].Kind
	})
	return pkg, nil
}

func extractDeclaration(modulePath string, decl ast.Decl, document parser.Syntax, comments commentIndex, info *check.ProgramInfo) []Symbol {
	groupDoc := comments.leading(decl.Span.Start.Offset)
	switch decl.Kind {
	case ast.DeclConst, ast.DeclVar:
		value := decl.Var
		kind := "var"
		objectKind := check.ObjectVar
		if decl.Kind == ast.DeclConst {
			value, kind = decl.Const, "const"
			objectKind = check.ObjectConst
		}
		out := make([]Symbol, 0, len(value.Names))
		for i, name := range value.Names {
			identifier := value.NameIDs[i]
			doc := comments.leading(identifier.Span.Start.Offset)
			if doc.Text == "" {
				doc = groupDoc
			}
			symbol := Symbol{Key: analysis.SymbolKey{ModulePath: modulePath, Kind: objectKind, Name: name}, Kind: kind, Name: name, Exported: exported(name), Doc: doc, Span: identifier.Span}
			if object, ok := info.Lookup(info.PackageScope, name); ok {
				symbol.CanonicalType = types.FormatWithTable(info.TypeTable, object.Type)
				if kind == "const" {
					if exact, found := info.ConstObjects[object.ID]; found {
						symbol.Exact = exact.Text
					}
				}
			}
			start := identifier.Span.Start.Offset
			if len(value.NameIDs) != 0 {
				start = value.NameIDs[0].Span.Start.Offset
			}
			if start >= 0 && decl.Span.End.Offset >= start && decl.Span.End.Offset <= len(document.File.Text) {
				symbol.DisplaySignature = kind + " " + strings.TrimSpace(document.File.Text[start:decl.Span.End.Offset])
			}
			if symbol.DisplaySignature == kind+" "+name && kind == "const" && symbol.Exact != "" {
				symbol.DisplaySignature += " = " + symbol.Exact
			}
			symbol.Deprecated = deprecatedText(doc.Text)
			out = append(out, symbol)
		}
		return out
	case ast.DeclType:
		doc := comments.leading(decl.Type.NameID.Span.Start.Offset)
		if doc.Text == "" {
			doc = groupDoc
		}
		symbol := Symbol{Key: analysis.SymbolKey{ModulePath: modulePath, Kind: check.ObjectType, Name: decl.Type.Name}, Kind: "type", Name: decl.Type.Name, Exported: exported(decl.Type.Name), Doc: doc, Span: decl.Type.NameID.Span}
		if object, ok := info.Lookup(info.PackageScope, decl.Type.Name); ok {
			symbol.CanonicalType = types.FormatWithTable(info.TypeTable, object.Type)
		}
		end := decl.Type.Type.Span.End.Offset
		if end >= decl.Type.NameID.Span.Start.Offset && end <= len(document.File.Text) {
			symbol.DisplaySignature = "type " + strings.TrimSpace(document.File.Text[decl.Type.NameID.Span.Start.Offset:end])
			if decl.Type.Type.Kind == ast.TypeStruct || decl.Type.Type.Kind == ast.TypeInterface {
				if brace := strings.IndexByte(symbol.DisplaySignature, '{'); brace >= 0 {
					symbol.DisplaySignature = strings.TrimSpace(symbol.DisplaySignature[:brace]) + " { ... }"
				}
			}
		}
		symbol.Deprecated = deprecatedText(doc.Text)
		symbol.TypeParameters = extractTypeParameters(decl.Type.TypeParams, document)
		symbol.Members = extractMembers(decl.Type.Type, document, comments)
		return []Symbol{symbol}
	case ast.DeclFunc:
		doc := groupDoc
		receiver := receiverName(decl.Func.Receiver, document)
		kind := "func"
		if receiver != "" {
			kind = "method"
		}
		end := decl.Func.Body.Span.Start.Offset
		if end <= decl.Span.Start.Offset || end > len(document.File.Text) {
			end = decl.Span.End.Offset
		}
		signature := strings.TrimSpace(document.File.Text[decl.Span.Start.Offset:end])
		symbol := Symbol{Key: analysis.SymbolKey{ModulePath: modulePath, Kind: check.ObjectFunc, Name: decl.Func.Name, Receiver: receiver}, Kind: kind, Name: decl.Func.Name, Receiver: receiver, Exported: exported(decl.Func.Name), DisplaySignature: signature, Doc: doc, Deprecated: deprecatedText(doc.Text), Span: decl.Func.NameID.Span}
		symbol.TypeParameters = extractTypeParameters(decl.Func.TypeParams, document)
		if objectID, ok := info.Functions[decl.Func.NodeID]; ok {
			if object, found := info.Object(objectID); found {
				symbol.CanonicalType = types.FormatWithTable(info.TypeTable, object.Type)
			}
		} else if object, ok := info.Lookup(info.PackageScope, decl.Func.Name); ok {
			symbol.CanonicalType = types.FormatWithTable(info.TypeTable, object.Type)
		}
		return []Symbol{symbol}
	}
	return nil
}

func extractMembers(typ ast.TypeExpr, document parser.Syntax, comments commentIndex) []Member {
	var out []Member
	for _, field := range typ.Fields {
		name := field.Name
		if name == "" {
			name = embeddedName(field.Type, document)
		}
		memberSpan := sourceDeclarationSpan(document, field.Span.Start.Offset)
		doc := comments.leading(field.Span.Start.Offset)
		if trailing := comments.trailing(memberSpan); trailing.Text != "" {
			doc = trailing
		}
		signature := sourceSlice(document, memberSpan)
		tag := sourceTag(document, field.Span)
		if tag != "" && !strings.HasSuffix(signature, tag) {
			signature += " " + tag
		}
		out = append(out, Member{Kind: "field", Name: name, Signature: signature, Exported: exported(name), Embedded: field.Name == "", RawTag: tag, Tag: field.Tag, Doc: doc, Span: memberSpan})
	}
	for _, method := range typ.Methods {
		doc := comments.leading(method.NameID.Span.Start.Offset)
		out = append(out, Member{
			Kind:      "method",
			Name:      method.Name,
			Signature: sourceSlice(document, sourceDeclarationSpan(document, method.NameID.Span.Start.Offset)),
			Exported:  exported(method.Name),
			Doc:       doc,
			Span:      method.NameID.Span,
		})
	}
	return out
}

func extractTypeParameters(parameters []ast.TypeParam, document parser.Syntax) []TypeParameter {
	out := make([]TypeParameter, 0, len(parameters))
	for _, parameter := range parameters {
		out = append(out, TypeParameter{Name: parameter.Name, Constraint: sourceSlice(document, parameter.Constraint.Span)})
	}
	return out
}

func attachMethods(pkg *Package) {
	methods := make(map[string][]Symbol)
	out := make([]Symbol, 0, len(pkg.Symbols))
	for _, symbol := range pkg.Symbols {
		if symbol.Kind == "method" {
			name := strings.TrimPrefix(symbol.Receiver, "*")
			methods[name] = append(methods[name], symbol)
			continue
		}
		out = append(out, symbol)
	}
	pkg.Symbols = out
	for i := range pkg.Symbols {
		pkg.Symbols[i].Methods = append(pkg.Symbols[i].Methods, methods[pkg.Symbols[i].Name]...)
		sort.Slice(pkg.Symbols[i].Methods, func(a, b int) bool { return pkg.Symbols[i].Methods[a].Name < pkg.Symbols[i].Methods[b].Name })
	}
}

func sourceSlice(document parser.Syntax, span source.Span) string {
	if !span.Valid() || span.Start.Offset < 0 || span.End.Offset > len(document.File.Text) {
		return ""
	}
	return strings.TrimSpace(document.File.Text[span.Start.Offset:span.End.Offset])
}

func sourceTag(document parser.Syntax, span source.Span) string {
	for _, token := range document.Tokens {
		if token.Span.Start.Offset < span.End.Offset {
			continue
		}
		if token.Kind == tokenpkg.String {
			return token.Lexeme
		}
		if token.Kind == tokenpkg.Semicolon || token.Kind == tokenpkg.Rbrace {
			return ""
		}
	}
	return ""
}

func sourceDeclarationSpan(document parser.Syntax, start int) source.Span {
	end := start
	paren, bracket, brace := 0, 0, 0
	started := false
	for _, token := range document.Tokens {
		if token.Span.Start.Offset < start {
			continue
		}
		started = true
		switch token.Kind {
		case tokenpkg.Lparen:
			paren++
		case tokenpkg.Rparen:
			if paren > 0 {
				paren--
			}
		case tokenpkg.Lbrack:
			bracket++
		case tokenpkg.Rbrack:
			if bracket > 0 {
				bracket--
			}
		case tokenpkg.Lbrace:
			brace++
		case tokenpkg.Rbrace:
			if brace == 0 && paren == 0 && bracket == 0 {
				span, _ := document.File.Span(start, end)
				return span
			}
			if brace > 0 {
				brace--
			}
		case tokenpkg.Semicolon:
			if paren == 0 && bracket == 0 && brace == 0 {
				span, _ := document.File.Span(start, end)
				return span
			}
		}
		end = token.Span.End.Offset
	}
	if started && end >= start && end <= len(document.File.Text) {
		span, _ := document.File.Span(start, end)
		return span
	}
	return source.Span{}
}

func receiverName(field *ast.Field, document parser.Syntax) string {
	if field == nil {
		return ""
	}
	text := sourceSlice(document, field.Type.Span)
	text = strings.TrimSpace(strings.TrimPrefix(text, "*"))
	if bracket := strings.IndexByte(text, '['); bracket >= 0 {
		text = text[:bracket]
	}
	if dot := strings.LastIndexByte(text, '.'); dot >= 0 {
		text = text[dot+1:]
	}
	if strings.HasPrefix(sourceSlice(document, field.Type.Span), "*") {
		return "*" + text
	}
	return text
}

func embeddedName(typ ast.TypeExpr, document parser.Syntax) string {
	text := strings.TrimPrefix(sourceSlice(document, typ.Span), "*")
	if bracket := strings.IndexByte(text, '['); bracket >= 0 {
		text = text[:bracket]
	}
	if dot := strings.LastIndexByte(text, '.'); dot >= 0 {
		text = text[dot+1:]
	}
	return strings.TrimSpace(text)
}

func defaultImportName(modulePath string) string {
	modulePath = strings.TrimSuffix(modulePath, "/")
	if slash := strings.LastIndexByte(modulePath, '/'); slash >= 0 {
		return modulePath[slash+1:]
	}
	return modulePath
}

func exported(name string) bool {
	r, _ := utf8.DecodeRuneInString(strings.TrimPrefix(name, "*"))
	return r != utf8.RuneError && unicode.IsUpper(r)
}
