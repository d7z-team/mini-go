package language

import (
	"fmt"
	"sort"

	"github.com/d7z-team/mini-go/compiler/analysis"
	"github.com/d7z-team/mini-go/compiler/ast"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (e *Engine) index(snapshot *Snapshot) {
	for modulePath, pkg := range snapshot.packages {
		info := pkg.Checked.Info
		for i := range pkg.Occurrences {
			occurrence := &pkg.Occurrences[i]
			uri, document, ok := snapshot.Document(modulePath, occurrence.Name.Span.Start.File)
			if !ok {
				continue
			}
			key := occurrenceKey(modulePath, pkg.Checked.Program, info, *occurrence)
			indexed := indexedOccurrence{Occurrence: occurrence, ModulePath: modulePath, Key: key}
			snapshot.occurrences[uri] = append(snapshot.occurrences[uri], indexed)
			selected, err := document.Index.Range(occurrence.Name.Span.Start.Offset, occurrence.Name.Span.End.Offset)
			if err != nil {
				continue
			}
			location := Location{URI: uri, Range: selected}
			if key != "" && (occurrence.Role == ast.NameDefinition || occurrence.Role == ast.NameImport || occurrence.Role == ast.NameLabelDefinition) {
				snapshot.definitions[key] = location
			}
			if key != "" {
				snapshot.references[key] = append(snapshot.references[key], location)
			}
		}
	}
	for uri := range snapshot.occurrences {
		sort.SliceStable(snapshot.occurrences[uri], func(i, j int) bool {
			return snapshot.occurrences[uri][i].Name.Span.Start.Offset < snapshot.occurrences[uri][j].Name.Span.Start.Offset
		})
	}
}

func occurrenceKey(modulePath string, program ast.Program, info *check.ProgramInfo, occurrence analysis.Occurrence) string {
	if occurrence.Role == ast.NameLabelDefinition || occurrence.Role == ast.NameLabelReference {
		return labelKey(modulePath, program, occurrence)
	}
	if selection, ok := info.Selections[occurrence.Node]; ok && occurrence.Role == ast.NameSelector {
		if selection.Kind == check.SelectionMethod || selection.Kind == check.SelectionMethodExpression {
			return methodKey(selection.ModulePath, info, selection.DeclaringReceiver, selection.Name)
		}
		if selection.Kind == check.SelectionField {
			return fieldKey(modulePath, info, selection.Receiver, selection.Name)
		}
		if selection.ModulePath != "" {
			return "export|" + selection.ModulePath + "|" + selection.Name
		}
	}
	if occurrence.Role == ast.NameSelector {
		if typeInfo, ok := info.Types[occurrence.Node]; ok {
			if node, exists := info.TypeTable.Node(typeInfo.Type); exists && node.Identity.ModulePath != "" {
				return "export|" + node.Identity.ModulePath + "|" + string(node.Identity.DeclID)
			}
		}
	}
	object, ok := info.Objects[occurrence.Object]
	if !ok {
		return declarationKey(modulePath, program, info, occurrence)
	}
	if object.FunctionID != "" && object.Kind == check.ObjectFunc {
		return "function|" + modulePath + "|" + object.FunctionID
	}
	if object.Scope == info.PackageScope {
		return "export|" + modulePath + "|" + object.Name
	}
	return "object|" + modulePath + "|" + string(object.ID)
}

func declarationKey(modulePath string, program ast.Program, info *check.ProgramInfo, occurrence analysis.Occurrence) string {
	for _, file := range program.Files {
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclFunc && decl.NodeID == occurrence.Node && decl.Func.Receiver != nil {
				return methodKey(modulePath, info, info.Types[decl.Func.Receiver.Type.NodeID].Type, decl.Func.Name)
			}
			if decl.Kind != ast.DeclType {
				continue
			}
			owner := info.Types[decl.Type.Type.NodeID].Type
			if object, ok := info.Lookup(info.PackageScope, decl.Type.Name); ok {
				owner = object.Type
			}
			for _, field := range decl.Type.Type.Fields {
				if field.NodeID == occurrence.Node {
					return fieldKey(modulePath, info, owner, field.Name)
				}
			}
			for _, method := range decl.Type.Type.Methods {
				if method.NodeID == occurrence.Node {
					return methodKey(modulePath, info, owner, method.Name)
				}
			}
		}
	}
	return ""
}

func methodKey(modulePath string, info *check.ProgramInfo, receiver types.TypeRef, name string) string {
	if modulePath == "" {
		modulePath = info.ModulePath
	}
	view := info.Relations.View(receiver)
	if view.Shape() == types.Pointer {
		if elem, ok := view.Elem(); ok {
			receiver = elem
		}
	}
	return "method|" + modulePath + "|" + types.FormatWithTable(info.TypeTable, receiver) + "|" + name
}

func fieldKey(modulePath string, info *check.ProgramInfo, receiver types.TypeRef, name string) string {
	view := info.Relations.View(receiver)
	if view.Shape() == types.Pointer {
		if elem, ok := view.Elem(); ok {
			receiver = elem
		}
	}
	return "field|" + modulePath + "|" + types.FormatWithTable(info.TypeTable, receiver) + "|" + name
}

func labelKey(modulePath string, program ast.Program, occurrence analysis.Occurrence) string {
	owner := 0
	for _, file := range program.Files {
		if file.Path != occurrence.Name.Span.Start.File {
			continue
		}
		for _, decl := range file.Decls {
			if decl.Kind == ast.DeclFunc && decl.Func.Body.Span.ContainsOffset(occurrence.Name.Span.Start.Offset) {
				owner = decl.Func.Body.Span.Start.Offset
			}
		}
	}
	return fmt.Sprintf("label|%s|%s|%d|%s", modulePath, occurrence.Name.Span.Start.File, owner, occurrence.Name.Text)
}

func (s *Snapshot) Document(modulePath, path string) (DocumentURI, Document, bool) {
	for uri, document := range s.documents {
		if document.Identity.ModulePath == modulePath && document.Identity.Path == path {
			return uri, document, true
		}
	}
	return "", Document{}, false
}

func (e *Engine) addDiagnostic(snapshot *Snapshot, diagnosticSource source.Diagnostic) {
	var uri DocumentURI
	var document Document
	for candidate, value := range snapshot.documents {
		if value.Identity.Path == diagnosticSource.Primary.Start.File && value.Identity.ModulePath == diagnosticSource.ModulePath {
			uri, document = candidate, value
			break
		}
	}
	if uri == "" {
		snapshot.workspaceDiagnostics = source.NormalizeDiagnostics(append(snapshot.workspaceDiagnostics, diagnosticSource))
		return
	}
	selected, err := document.Index.Range(diagnosticSource.Primary.Start.Offset, diagnosticSource.Primary.End.Offset)
	if err != nil {
		selected = Range{}
	}
	severity := 1
	switch diagnosticSource.Severity {
	case source.SeverityWarning:
		severity = 2
	case source.SeverityInformation:
		severity = 3
	case source.SeverityHint:
		severity = 4
	}
	diagnostic := Diagnostic{
		Range: selected, Severity: severity, Code: string(diagnosticSource.Code), Source: "mini-go", Message: diagnosticSource.Message,
	}
	for _, existing := range snapshot.diagnostics[uri] {
		if existing.Code == diagnostic.Code && existing.Message == diagnostic.Message && existing.Range == diagnostic.Range {
			return
		}
	}
	snapshot.diagnostics[uri] = append(snapshot.diagnostics[uri], diagnostic)
	if snapshot.editable[uri] {
		return
	}
	// Project dependency failures to the editable import that reaches them.
	for modulePath, pkg := range snapshot.packages {
		for _, file := range pkg.Checked.Program.Files {
			importURI, importing, ok := snapshot.Document(modulePath, file.Path)
			if !ok || !snapshot.editable[importURI] {
				continue
			}
			for _, decl := range file.Decls {
				if decl.Kind != ast.DeclImport {
					continue
				}
				pending := []string{decl.Import.Path}
				seen := map[string]bool{}
				reaches := false
				for len(pending) > 0 {
					path := pending[len(pending)-1]
					pending = pending[:len(pending)-1]
					if path == diagnosticSource.ModulePath {
						reaches = true
						break
					}
					if seen[path] {
						continue
					}
					seen[path] = true
					for _, dependencyFile := range snapshot.packages[path].Checked.Program.Files {
						for _, dependency := range dependencyFile.Decls {
							if dependency.Kind == ast.DeclImport {
								pending = append(pending, dependency.Import.Path)
							}
						}
					}
				}
				if !reaches {
					continue
				}
				span := decl.Import.PathSpan
				rangeAtImport, err := importing.Index.Range(span.Start.Offset, span.End.Offset)
				if err != nil {
					continue
				}
				projected := diagnostic
				projected.Range = rangeAtImport
				projected.RelatedInformation = []DiagnosticRelatedInformation{{Location: Location{URI: uri, Range: diagnostic.Range}, Message: diagnostic.Message}}
				snapshot.diagnostics[importURI] = append(snapshot.diagnostics[importURI], projected)
			}
		}
	}
}

func (e *Engine) occurrence(uri DocumentURI, position Position) (indexedOccurrence, bool) {
	document, ok := e.snapshot.documents[uri]
	if !ok {
		return indexedOccurrence{}, false
	}
	offset, err := document.Index.Offset(position)
	if err != nil {
		return indexedOccurrence{}, false
	}
	var selected indexedOccurrence
	found := false
	for _, occurrence := range e.snapshot.occurrences[uri] {
		span := occurrence.Name.Span
		if offset >= span.Start.Offset && offset < span.End.Offset {
			if !found || span.End.Offset-span.Start.Offset < selected.Name.Span.End.Offset-selected.Name.Span.Start.Offset {
				selected, found = occurrence, true
			}
		}
	}
	return selected, found
}
