package workspace

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/d7z-team/mini-go/compiler/source"
)

func normalizePackage(pkg SourcePackage) (SourcePackage, error) {
	modulePath, err := normalizeModulePath(pkg.ModulePath)
	if err != nil {
		return SourcePackage{}, err
	}
	if len(pkg.Files) == 0 {
		return SourcePackage{}, fmt.Errorf("source package %q has no production files", modulePath)
	}
	packageID, err := normalizePackageID(pkg.ID, modulePath)
	if err != nil {
		return SourcePackage{}, err
	}
	out := SourcePackage{
		ID: packageID, ModulePath: modulePath, SelectionTarget: pkg.SelectionTarget,
		SourceCandidates: append([]SourceCandidate(nil), pkg.SourceCandidates...),
	}
	seen := map[string]struct{}{}
	appendFiles := func(files []source.File, tests bool) error {
		for _, file := range files {
			name, err := normalizeFilePath(file.Path)
			if err != nil {
				return err
			}
			if !isSourceFile(name) {
				return fmt.Errorf("source file %q must use .mgo", name)
			}
			if file.OriginPath != "" {
				file.OriginPath, err = normalizeFilePath(file.OriginPath)
				if err != nil {
					return fmt.Errorf("source origin: %w", err)
				}
			}
			if isTestFile(name) != tests {
				return fmt.Errorf("source file %q is in the wrong production/test set", name)
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate source file %q in package %q", name, modulePath)
			}
			seen[name] = struct{}{}
			file.Path = name
			file.ID = "file:" + modulePath + "/" + name
			if tests {
				out.TestFiles = append(out.TestFiles, file)
			} else {
				out.Files = append(out.Files, file)
			}
		}
		return nil
	}
	if err := appendFiles(pkg.Files, false); err != nil {
		return SourcePackage{}, err
	}
	if err := appendFiles(pkg.TestFiles, true); err != nil {
		return SourcePackage{}, err
	}
	resourceSeen := map[string]struct{}{}
	for _, resource := range pkg.Resources {
		name, err := normalizeFilePath(resource.Path)
		if err != nil {
			return SourcePackage{}, fmt.Errorf("invalid resource: %w", err)
		}
		if _, exists := resourceSeen[name]; exists {
			return SourcePackage{}, fmt.Errorf("duplicate resource file %q in package %q", name, modulePath)
		}
		resourceSeen[name] = struct{}{}
		resource.Path = name
		resource.Data = append([]byte(nil), resource.Data...)
		if resource.Hash == "" {
			resource.Hash = source.HashText(string(resource.Data))
		}
		out.Resources = append(out.Resources, resource)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	sort.Slice(out.TestFiles, func(i, j int) bool { return out.TestFiles[i].Path < out.TestFiles[j].Path })
	sort.Slice(out.Resources, func(i, j int) bool { return out.Resources[i].Path < out.Resources[j].Path })
	return out, nil
}

func normalizeModulePath(modulePath string) (string, error) {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" || strings.Contains(modulePath, "\\") || strings.HasPrefix(modulePath, "/") || strings.ContainsFunc(modulePath, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || path.Clean(modulePath) != modulePath {
		return "", fmt.Errorf("invalid module path %q", modulePath)
	}
	for _, part := range strings.Split(modulePath, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid module path %q", modulePath)
		}
	}
	return modulePath, nil
}

func normalizeFilePath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return "", fmt.Errorf("invalid source path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid source path %q", name)
		}
	}
	return name, nil
}

func hasHiddenPathElement(name string) bool {
	for _, element := range strings.Split(name, "/") {
		if strings.HasPrefix(element, ".") || strings.HasPrefix(element, "_") {
			return true
		}
	}
	return false
}

type packageFileClass struct {
	source          bool
	test            bool
	resource        bool
	discoverPackage bool
}

func classifyPackageFile(name string) packageFileClass {
	if strings.HasSuffix(name, ".mrpc") {
		return packageFileClass{}
	}
	sourceFile := strings.HasSuffix(name, ".mgo")
	return packageFileClass{
		source:          sourceFile,
		test:            sourceFile && strings.HasSuffix(name, "_test.mgo"),
		resource:        true,
		discoverPackage: sourceFile && !hasHiddenPathElement(name),
	}
}

func isSourceFile(name string) bool {
	return classifyPackageFile(name).source
}

func isTestFile(name string) bool {
	return classifyPackageFile(name).test
}

func clonePackage(pkg SourcePackage) SourcePackage {
	pkg.Files = append([]source.File(nil), pkg.Files...)
	pkg.TestFiles = append([]source.File(nil), pkg.TestFiles...)
	for i := range pkg.Files {
		pkg.Files[i].LineStarts = append([]int(nil), pkg.Files[i].LineStarts...)
	}
	for i := range pkg.TestFiles {
		pkg.TestFiles[i].LineStarts = append([]int(nil), pkg.TestFiles[i].LineStarts...)
	}
	pkg.Resources = append([]ResourceFile(nil), pkg.Resources...)
	for i := range pkg.Resources {
		pkg.Resources[i].Data = append([]byte(nil), pkg.Resources[i].Data...)
	}
	pkg.SelectionTarget.Tags = append([]string(nil), pkg.SelectionTarget.Tags...)
	pkg.SourceCandidates = append([]SourceCandidate(nil), pkg.SourceCandidates...)
	return pkg
}
