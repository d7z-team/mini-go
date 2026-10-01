package language

import "strings"

func (e *Engine) Diagnostics(uri DocumentURI, previousResultID string) DiagnosticReport {
	resultID := e.snapshot.resultIDs[uri]
	if resultID != "" && resultID == previousResultID {
		return DiagnosticReport{Kind: "unchanged", ResultID: resultID}
	}
	items := e.snapshot.Diagnostics(uri)
	return DiagnosticReport{Kind: "full", ResultID: resultID, Items: items}
}

func (e *Engine) Definition(uri DocumentURI, position Position) []Location {
	occurrence, ok := e.occurrence(uri, position)
	if !ok {
		return nil
	}
	if location, ok := e.snapshot.definitions[occurrence.Key]; ok {
		return []Location{location}
	}
	return nil
}

func (e *Engine) References(uri DocumentURI, position Position, includeDeclaration bool) []Location {
	occurrence, ok := e.occurrence(uri, position)
	if !ok {
		return nil
	}
	values := append([]Location(nil), e.snapshot.references[occurrence.Key]...)
	if includeDeclaration {
		return values
	}
	definition := e.snapshot.definitions[occurrence.Key]
	out := values[:0]
	for _, location := range values {
		if location != definition {
			out = append(out, location)
		}
	}
	return out
}

func (e *Engine) Hover(uri DocumentURI, position Position) *Hover {
	occurrence, ok := e.occurrence(uri, position)
	if !ok {
		return nil
	}
	document := e.snapshot.documents[uri]
	selected, _ := document.Index.Range(occurrence.Name.Span.Start.Offset, occurrence.Name.Span.End.Offset)
	detail := occurrence.Name.Text
	if occurrence.TypeText != "" {
		detail += " " + occurrence.TypeText
	}
	markdown := "```go\n" + detail + "\n```"
	if documentation := e.documentation(occurrence); documentation != "" {
		markdown += "\n\n" + documentation
	}
	return &Hover{Contents: MarkupContent{Kind: "markdown", Value: markdown}, Range: &selected}
}

func (e *Engine) documentation(occurrence indexedOccurrence) string {
	if occurrence.Occurrence == nil {
		return ""
	}
	modulePath := occurrence.Symbol.ModulePath
	if modulePath == "" {
		modulePath = occurrence.ModulePath
	}
	receiver := ""
	parts := strings.Split(occurrence.Key, "|")
	if len(parts) >= 4 && (parts[0] == "method" || parts[0] == "field") {
		modulePath, receiver = parts[1], parts[2]
	}
	sourceDoc, ok := e.snapshot.documentation.LookupComment(modulePath, receiver, occurrence.Name.Text)
	if !ok {
		return ""
	}
	return e.snapshot.renderComment(modulePath, sourceDoc)
}
