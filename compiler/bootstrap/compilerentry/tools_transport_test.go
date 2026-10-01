package compilerentry

import (
	"encoding/binary"
	"testing"
)

func TestToolsResponseFramePreservesRawSegmentsAndOwnsInput(t *testing.T) {
	want := ToolsResponse{Format: ToolsFormat, Version: ToolsVersion, CompilerID: "compiler", Session: "session", Revision: "revision", ImageJSON: "{\"text\":\"<>&中文\\n\"}\n", SymbolsJSON: "{\"symbols\":[]}\n"}
	encoded, err := encodeToolsResponse(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeToolsResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	clear(encoded)
	if got.ImageJSON != want.ImageJSON || got.SymbolsJSON != want.SymbolsJSON || got.Session != want.Session || got.Revision != want.Revision {
		t.Fatalf("response changed: %#v", got)
	}
}

func TestToolsResponseFrameRejectsMalformedInput(t *testing.T) {
	encoded, err := encodeToolsResponse(ToolsResponse{Format: ToolsFormat, Version: ToolsVersion, ImageJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"magic", "truncated header", "truncated segment", "trailing", "length overflow", "invalid UTF-8"} {
		t.Run(name, func(t *testing.T) {
			input := append([]byte(nil), encoded...)
			switch name {
			case "magic":
				input[0]++
			case "truncated header":
				input = input[:19]
			case "truncated segment":
				input = input[:len(input)-1]
			case "trailing":
				input = append(input, 0)
			case "length overflow":
				binary.LittleEndian.PutUint32(input[8:12], ^uint32(0))
			case "invalid UTF-8":
				input[len(input)-1] = 0xff
			}
			if _, err := DecodeToolsResponse(input); err == nil {
				t.Fatal("malformed frame accepted")
			}
		})
	}
}

func TestToolsCallFramesStrictRequestErrors(t *testing.T) {
	var service ToolService
	for _, input := range []string{`{`, `{"Format":"mini-go-tools","Version":3,"Unknown":true}`, `{} {}`} {
		response, err := DecodeToolsResponse(service.Call([]byte(input)))
		if err != nil {
			t.Fatal(err)
		}
		if response.Error == nil || response.Error.Code != "invalid_argument" {
			t.Fatalf("invalid input accepted: %#v", response)
		}
	}
}
