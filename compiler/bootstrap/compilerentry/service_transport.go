package compilerentry

import (
	"bytes"
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// encodeResponse keeps the large executable and symbol graphs on their fixed
// schema codecs. Only the small service metadata uses general JSON reflection.
func encodeResponse(response Response) ([]byte, error) {
	metadata := struct {
		Format      string
		Version     int
		Diagnostics []source.Diagnostic
		Tests       []cache.TestEntry
		CompilerID  string
		Stats       compiler.Stats
		CacheStats  cache.TransientStats
		Error       string
	}{
		response.Format, response.Version, response.Diagnostics, response.Tests,
		response.CompilerID, response.Stats, response.CacheStats, response.Error,
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(metadata); err != nil {
		return nil, err
	}
	// Encoder writes a complete object and its framing newline. The two
	// remaining fields are emitted directly, without a RawMessage re-scan.
	output := buffer.Bytes()
	output = append(output[:len(output)-2], `,"Image":`...)
	if response.Image == nil {
		output = append(output, "null"...)
	} else {
		image, err := ir.EncodeExecutionImage(response.Image)
		if err != nil {
			return nil, err
		}
		output = append(output, image...)
	}
	output = append(output, `,"Symbols":`...)
	if response.Symbols == nil {
		output = append(output, "null"...)
	} else {
		symbols, err := ir.EncodeProgramSymbols(response.Symbols)
		if err != nil {
			return nil, err
		}
		output = append(output, symbols...)
	}
	output = append(output, '}', '\n')
	return output, nil
}
