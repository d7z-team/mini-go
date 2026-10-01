package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

func writeSnapshot(path string, value snapshot) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	writer.Header.ModTime = time.Time{}
	writer.Header.OS = 255
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, compressed.Bytes(), 0o644)
}

func readSnapshot(path string) (snapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return snapshot{}, err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return snapshot{}, err
	}
	defer reader.Close()
	var out snapshot
	if err := json.NewDecoder(reader).Decode(&out); err != nil {
		return snapshot{}, err
	}
	if out.Schema != schema || out.Version != version || out.GoVersion != goVersion && out.GoVersion != jsonGoVersion {
		return snapshot{}, errors.New("core API baseline has an unsupported schema or Go version")
	}
	seen := make(map[string]bool)
	for _, pkg := range out.Packages {
		if pkg.Path == "" || seen[pkg.Path] || (pkg.Path == "encoding/json") != (out.GoVersion == jsonGoVersion) {
			return snapshot{}, errors.New("core API baseline has invalid or duplicate package coverage")
		}
		seen[pkg.Path] = true
	}
	if len(seen) == 0 {
		return snapshot{}, errors.New("core API baseline is empty")
	}
	return out, nil
}
