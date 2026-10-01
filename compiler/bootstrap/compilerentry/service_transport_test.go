package compilerentry

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/source"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestResponseEncodingPreservesMetadataAndCanonicalImageTokens(t *testing.T) {
	for _, withImage := range []bool{false, true} {
		response := Response{
			Format: ServiceFormat, Version: ServiceVersion, CompilerID: "compiler",
			Diagnostics: []source.Diagnostic{{Code: "sample", Message: "<>&中文"}},
			Stats:       compiler.Stats{PackagesParsed: 3}, Error: "quoted \"field\"",
		}
		if withImage {
			response.Image = &ir.ExecutionImage{
				Format: ir.ExecutionFormat, Version: ir.ExecutionVersion,
				Packages: map[string]ir.PackageArchive{"sample": {
					Artifact: json.RawMessage(`{"integer":9007199254740993,"text":"<>&中文"}`), ArtifactHash: "hash",
				}},
				Hash: "image",
			}
			response.Symbols = &ir.ProgramSymbols{Hash: "symbols"}
		}
		encoded, err := encodeResponse(response)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Response
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, response) {
			t.Fatalf("response changed: %#v", decoded)
		}
		if withImage {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			image, err := ir.EncodeExecutionImage(response.Image)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fields["Image"], image) {
				t.Fatal("image token spelling changed")
			}
		}
	}
}

func TestResponseEncodingRejectsMalformedArchiveJSON(t *testing.T) {
	response := Response{Image: &ir.ExecutionImage{Packages: map[string]ir.PackageArchive{
		"sample": {Artifact: json.RawMessage(`{"unfinished":`)},
	}}}
	if _, err := encodeResponse(response); err == nil {
		t.Fatal("invalid archive JSON was emitted")
	}
}
