package workspace

import (
	"fmt"
	"path"
	"strings"

	"github.com/d7z-team/mini-go/compiler/source"
)

type TreeFile struct {
	Path string
	Text string
	Data []byte
}

func NewTreeSourceSet(modulePath string, files []TreeFile) (MemorySourceSet, error) {
	modulePath, err := normalizeModulePath(modulePath)
	if err != nil {
		return MemorySourceSet{}, err
	}
	return newTreeSourceSet(modulePath, files)
}

// NewPackageTreeSourceSet maps each source directory directly to its import path.
// It is intended for embedded source collections such as the standard library.
func NewPackageTreeSourceSet(files []TreeFile) (MemorySourceSet, error) {
	return newTreeSourceSet("", files)
}

func newTreeSourceSet(modulePath string, files []TreeFile) (MemorySourceSet, error) {
	packages := map[string]*SourcePackage{}
	type treeEntry struct {
		name string
		data []byte
	}
	entries := make([]treeEntry, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, treeFile := range files {
		name, err := normalizeFilePath(treeFile.Path)
		if err != nil {
			return MemorySourceSet{}, err
		}
		if _, exists := seen[name]; exists {
			return MemorySourceSet{}, fmt.Errorf("duplicate source file %q", name)
		}
		seen[name] = struct{}{}
		metadata := false
		for _, part := range strings.Split(name, "/") {
			if part == ".git" || part == ".hg" || part == ".svn" || part == ".bzr" {
				metadata = true
				break
			}
		}
		if metadata {
			continue
		}
		data := treeFile.Data
		if treeFile.Data == nil {
			data = []byte(treeFile.Text)
		}
		entries = append(entries, treeEntry{name: name, data: data})
	}
	for _, entry := range entries {
		name, data := entry.name, entry.data
		classification := classifyPackageFile(name)
		if !classification.discoverPackage {
			continue
		}
		dir := path.Dir(name)
		packagePath := modulePath
		if packagePath == "" {
			if dir == "." {
				continue
			}
			packagePath = dir
		} else if dir != "." {
			packagePath += "/" + dir
		}
		pkg := packages[packagePath]
		if pkg == nil {
			packageID := PackageID{Namespace: "std", Path: packagePath}
			if modulePath != "" {
				relative := strings.TrimPrefix(packagePath, modulePath)
				packageID = PackageID{Namespace: "module:" + modulePath, Path: strings.TrimPrefix(relative, "/")}
			}
			pkg = &SourcePackage{ID: packageID, ModulePath: packagePath}
			packages[packagePath] = pkg
		}
		file := source.File{Path: name, Text: string(data)}
		if classification.test {
			pkg.TestFiles = append(pkg.TestFiles, file)
		} else {
			pkg.Files = append(pkg.Files, file)
		}
	}
	packageDirs := make(map[string]string, len(packages))
	for packagePath := range packages {
		dir := strings.TrimPrefix(packagePath, modulePath)
		dir = strings.TrimPrefix(dir, "/")
		if modulePath == "" {
			dir = packagePath
		}
		if dir == "" {
			dir = "."
		}
		packageDirs[packagePath] = dir
	}
	for packagePath, packageDir := range packageDirs {
		for _, entry := range entries {
			classification := classifyPackageFile(entry.name)
			if !classification.resource {
				continue
			}
			name := entry.name
			if packageDir != "." {
				if !strings.HasPrefix(name, packageDir+"/") {
					continue
				}
				name = strings.TrimPrefix(name, packageDir+"/")
			}
			// NewMemorySourceSet takes the final owned copy during normalization.
			packages[packagePath].Resources = append(packages[packagePath].Resources, ResourceFile{
				Path: name, Data: entry.data, Hash: source.HashText(string(entry.data)),
			})
		}
	}
	out := make([]SourcePackage, 0, len(packages))
	for _, pkg := range packages {
		out = append(out, *pkg)
	}
	return NewMemorySourceSet(out)
}
