package language

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/analysis"
	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (e *Engine) Completion(uri DocumentURI, position Position) CompletionList {
	document, ok := e.snapshot.documents[uri]
	if !ok {
		return CompletionList{}
	}
	pkg, ok := e.snapshot.packages[document.Identity.ModulePath]
	if !ok {
		return CompletionList{}
	}
	offset, err := document.Index.Offset(position)
	if err != nil {
		return CompletionList{}
	}
	info := pkg.Checked.Info
	if items, selector := e.selectorCompletion(document, pkg, offset); selector {
		return CompletionList{Items: items}
	}
	activeScope := info.PackageScope
	for _, candidate := range e.snapshot.occurrences[uri] {
		if candidate.Name.Span.Start.Offset > offset || candidate.Object == "" {
			continue
		}
		if object, exists := info.Objects[candidate.Object]; exists && object.Scope != 0 {
			activeScope = object.Scope
		}
	}
	visibleScopes := map[check.ScopeID]bool{}
	for scopeID := activeScope; scopeID != 0; {
		visibleScopes[scopeID] = true
		scope := info.Scopes[scopeID]
		if scope == nil {
			break
		}
		scopeID = scope.Parent
	}
	seen := map[string]bool{}
	var items []CompletionItem
	for _, object := range info.Objects {
		if object.Name == "_" || seen[object.Name] || !visibleScopes[object.Scope] {
			continue
		}
		if object.Definition.Span.Valid() && object.Definition.Span.Start.File == document.Identity.Path && object.Definition.Span.Start.Offset > offset {
			continue
		}
		seen[object.Name] = true
		detail := ""
		if object.Type.Valid() {
			detail = types.FormatWithTable(info.TypeTable, object.Type)
		}
		item := CompletionItem{
			Label: object.Name, Kind: objectSymbolKind(object.Kind), Detail: detail, SortText: object.Name,
			Data: fmt.Sprintf("%d|%s|%s", e.snapshot.generation, document.Identity.ModulePath, object.ID),
		}
		if object.Scope == info.PackageScope {
			if sourceDoc, found := e.snapshot.documentation.LookupComment(document.Identity.ModulePath, "", object.Name); found {
				item.Documentation = MarkupContent{Kind: "markdown", Value: e.snapshot.renderComment(document.Identity.ModulePath, sourceDoc)}
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	return CompletionList{Items: items}
}

func (e *Engine) ResolveCompletion(item CompletionItem) CompletionItem {
	parts := strings.SplitN(item.Data, "|", 3)
	if len(parts) == 3 {
		generation, err := strconv.ParseUint(parts[0], 10, 64)
		pkg, exists := e.snapshot.packages[parts[1]]
		if err == nil && generation == e.snapshot.generation && exists {
			object, found := pkg.Checked.Info.Objects[check.ObjectID(parts[2])]
			if !found {
				return item
			}
			item.Detail = types.FormatWithTable(pkg.Checked.Info.TypeTable, object.Type)
			if sourceDoc, ok := e.snapshot.documentation.LookupComment(parts[1], "", object.Name); ok {
				item.Documentation = MarkupContent{Kind: "markdown", Value: e.snapshot.renderComment(parts[1], sourceDoc)}
			}
		}
	}
	if item.Documentation.Value == "" && item.Detail != "" {
		item.Documentation = MarkupContent{Kind: "markdown", Value: "```go\n" + item.Label + " " + item.Detail + "\n```"}
	}
	return item
}

func (e *Engine) selectorCompletion(document Document, pkg analysis.Package, offset int) ([]CompletionItem, bool) {
	dot := offset - 1
	for dot >= 0 && (document.Text[dot] == ' ' || document.Text[dot] == '\t') {
		dot--
	}
	if dot < 0 || document.Text[dot] != '.' {
		return nil, false
	}
	var receiver indexedOccurrence
	for _, occurrence := range e.snapshot.occurrences[document.Identity.URI] {
		if occurrence.Name.Span.End.Offset == dot && occurrence.Type.Valid() {
			receiver = occurrence
			break
		}
	}
	if receiver.Occurrence == nil || !receiver.Type.Valid() {
		return nil, true
	}
	info := pkg.Checked.Info
	ref := receiver.Type
	if info.Relations.View(ref).Shape() == types.Pointer {
		ref, _ = info.Relations.View(ref).Elem()
	}
	seen := map[string]bool{}
	var items []CompletionItem
	if fields, ok := info.Relations.View(ref).StructFields(); ok {
		for _, field := range fields {
			if field.Name == "" || seen[field.Name] {
				continue
			}
			seen[field.Name] = true
			items = append(items, CompletionItem{Label: field.Name, Kind: 5, Detail: types.FormatWithTable(info.TypeTable, field.Type), SortText: field.Name})
		}
	}
	if node, ok := info.TypeTable.Node(ref); ok {
		for _, method := range node.Methods {
			if method.Name == "" || seen[method.Name] {
				continue
			}
			seen[method.Name] = true
			items = append(items, CompletionItem{Label: method.Name, Kind: 2, Detail: types.FormatSignature(info.TypeTable, method.Signature), SortText: method.Name})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Label < items[j].Label })
	return items, true
}

func objectSymbolKind(kind check.ObjectKind) int {
	switch kind {
	case check.ObjectFunc:
		return 3
	case check.ObjectVar:
		return 6
	case check.ObjectConst:
		return 14
	case check.ObjectType, check.ObjectTypeParam:
		return 7
	case check.ObjectField:
		return 5
	default:
		return 13
	}
}

func (e *Engine) SignatureHelp(uri DocumentURI, position Position) *SignatureHelp {
	document, ok := e.snapshot.documents[uri]
	if !ok {
		return nil
	}
	offset, err := document.Index.Offset(position)
	if err != nil {
		return nil
	}
	pkg, ok := e.snapshot.packages[document.Identity.ModulePath]
	if !ok {
		return nil
	}
	var call *ast.Expression
	ast.WalkExpressions(&pkg.Checked.Program, func(expression *ast.Expression) {
		if expression.Kind != ast.ExprCall || expression.Callee == nil || expression.Span.Start.File != document.Identity.Path ||
			offset < expression.Callee.Span.End.Offset || offset > expression.Span.End.Offset {
			return
		}
		if call == nil || expression.Span.End.Offset-expression.Span.Start.Offset < call.Span.End.Offset-call.Span.Start.Offset {
			call = expression
		}
	})
	if call == nil {
		return nil
	}
	info := pkg.Checked.Info
	calleeInfo, ok := info.Exprs[call.Callee.NodeID]
	if !ok {
		return nil
	}
	signature, callable := info.Relations.View(calleeInfo.Type).Function()
	if !callable {
		return nil
	}
	active := 0
	for _, argument := range call.Args {
		if argument.Span.Start.Offset >= offset {
			break
		}
		active++
		if offset <= argument.Span.End.Offset {
			active--
			break
		}
	}
	if len(signature.Params) != 0 && active >= len(signature.Params) {
		active = len(signature.Params) - 1
	}
	label := strings.TrimSpace(document.Text[call.Callee.Span.Start.Offset:call.Callee.Span.End.Offset])
	signatureInfo := SignatureInformation{Label: label + " " + types.FormatSignature(info.TypeTable, signature)}
	var candidate indexedOccurrence
	for _, occurrence := range e.snapshot.occurrences[uri] {
		if occurrence.Name.Span.Start.Offset >= call.Callee.Span.Start.Offset && occurrence.Name.Span.End.Offset <= call.Callee.Span.End.Offset {
			candidate = occurrence
		}
	}
	if documentation := e.documentation(candidate); documentation != "" {
		signatureInfo.Documentation = MarkupContent{Kind: "markdown", Value: documentation}
	}
	return &SignatureHelp{Signatures: []SignatureInformation{signatureInfo}, ActiveParameter: active}
}
