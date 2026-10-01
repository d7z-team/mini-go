package workspace

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/parser"
	"github.com/d7z-team/mini-go/compiler/source"
)

func resolveEmbeds(builder parser.PackageBuilder, resources []ResourceFile) []source.Diagnostic {
	var diagnostics []source.Diagnostic
	for _, decl := range builder.Embeds() {
		add := func(code, message string) {
			diagnostics = append(diagnostics, source.Diagnostic{
				Code: source.DiagnosticCode(code), Severity: source.SeverityError,
				Message: message, Primary: decl.Span,
			})
		}
		if !decl.ImportsEmbed {
			add("compiler.embed.import", "//go:embed requires importing package embed")
			continue
		}
		if !decl.ValidVariable {
			add("compiler.embed.declaration", "//go:embed requires one package variable without an initializer")
			continue
		}
		matched, err := matchEmbedResources(decl.Patterns, resources)
		if err != nil {
			add("compiler.embed.pattern", err.Error())
			continue
		}
		files := make([]ast.EmbedFile, len(matched))
		for index, resource := range matched {
			files[index] = ast.EmbedFile{Path: resource.Path, Data: resource.Data}
		}
		if !builder.BindEmbed(decl.File, decl.Declaration, files) {
			add("compiler.embed.declaration", "invalid embed declaration")
		}
	}
	return diagnostics
}

func matchEmbedResources(patterns []string, resources []ResourceFile) ([]ResourceFile, error) {
	directories := map[string]struct{}{}
	for _, resource := range resources {
		for dir := path.Dir(resource.Path); dir != "."; dir = path.Dir(dir) {
			directories[dir] = struct{}{}
		}
	}
	candidates := make([]string, 0, len(resources)+len(directories))
	directory := make(map[string]bool, len(directories))
	for name := range directories {
		candidates = append(candidates, name)
		directory[name] = true
	}
	for _, resource := range resources {
		candidates = append(candidates, resource.Path)
	}
	sort.Strings(candidates)
	byPath := make(map[string]ResourceFile, len(resources))
	for _, resource := range resources {
		byPath[resource.Path] = resource
	}
	matches := map[string]ResourceFile{}
	for _, original := range patterns {
		pattern := strings.TrimSpace(original)
		includeHidden := strings.HasPrefix(pattern, "all:")
		pattern = strings.TrimPrefix(pattern, "all:")
		if err := validateEmbedPattern(pattern); err != nil {
			return nil, err
		}
		matched := false
		for _, candidate := range candidates {
			ok, err := path.Match(pattern, candidate)
			if err != nil {
				return nil, fmt.Errorf("invalid //go:embed pattern %q: %v", original, err)
			}
			if !ok {
				continue
			}
			if !directory[candidate] {
				if err := validateEmbedMatch(candidate); err != nil {
					return nil, fmt.Errorf("invalid //go:embed match %q: %v", candidate, err)
				}
				matched = true
				matches[candidate] = byPath[candidate]
				continue
			}
			prefix := candidate + "/"
			for _, resource := range resources {
				if !strings.HasPrefix(resource.Path, prefix) {
					continue
				}
				relative := strings.TrimPrefix(resource.Path, prefix)
				if !includeHidden && hasHiddenPathElement(relative) {
					continue
				}
				if err := validateEmbedMatch(resource.Path); err != nil {
					return nil, fmt.Errorf("invalid //go:embed match %q: %v", resource.Path, err)
				}
				matched = true
				matches[resource.Path] = resource
			}
		}
		if !matched {
			return nil, fmt.Errorf("//go:embed pattern %q matched no files", original)
		}
	}
	out := make([]ResourceFile, 0, len(matches))
	for _, resource := range matches {
		out = append(out, resource)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func validateEmbedMatch(name string) error {
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "vendor", ".git", ".hg", ".svn", ".bzr":
			return fmt.Errorf("path contains excluded directory %q", part)
		}
		if strings.ContainsAny(part, ":*?\"<>|`'") {
			return fmt.Errorf("path element %q contains unsupported punctuation", part)
		}
	}
	return nil
}

func validateEmbedPattern(pattern string) error {
	if pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") {
		return fmt.Errorf("invalid //go:embed pattern %q", pattern)
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid //go:embed pattern %q", pattern)
		}
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("invalid //go:embed pattern %q: %v", pattern, err)
	}
	return nil
}
