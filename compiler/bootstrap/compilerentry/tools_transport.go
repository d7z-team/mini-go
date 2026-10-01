package compilerentry

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// The response frame owns all its bytes: magic, three little-endian u32
// lengths, metadata JSON, image JSON, symbols JSON. Images are not JSON strings
// inside metadata and therefore do not undergo another quoting pass.
const (
	toolsResponseMagic  = "MGT3\r\n\x1a\n"
	toolsResponseHeader = 20
)

var errToolsOutputBudget = errors.New("tool output exceeds byte limit")

func encodeToolsResponse(response ToolsResponse) ([]byte, error) {
	image, symbols := response.ImageJSON, response.SymbolsJSON
	response.ImageJSON, response.SymbolsJSON = "", ""
	var metadata bytes.Buffer
	encoder := json.NewEncoder(&metadata)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return nil, err
	}
	total := uint64(toolsResponseHeader) + uint64(metadata.Len()) + uint64(len(image)) + uint64(len(symbols))
	if total > MaxToolsInput {
		return nil, errToolsOutputBudget
	}
	output := make([]byte, toolsResponseHeader, int(total))
	copy(output, toolsResponseMagic)
	binary.LittleEndian.PutUint32(output[8:12], uint32(metadata.Len()))
	binary.LittleEndian.PutUint32(output[12:16], uint32(len(image)))
	binary.LittleEndian.PutUint32(output[16:20], uint32(len(symbols)))
	output = append(output, metadata.Bytes()...)
	output = append(output, image...)
	output = append(output, symbols...)
	return output, nil
}

// DecodeToolsResponse decodes the versioned portable Tools response. It does
// not retain the caller's buffer or validate the separately sealed image.
func DecodeToolsResponse(input []byte) (ToolsResponse, error) {
	if len(input) < toolsResponseHeader || len(input) > MaxToolsInput || string(input[:8]) != toolsResponseMagic {
		return ToolsResponse{}, errors.New("invalid tools response frame")
	}
	metadata := uint64(binary.LittleEndian.Uint32(input[8:12]))
	image := uint64(binary.LittleEndian.Uint32(input[12:16]))
	symbols := uint64(binary.LittleEndian.Uint32(input[16:20]))
	if uint64(toolsResponseHeader)+metadata+image+symbols != uint64(len(input)) {
		return ToolsResponse{}, errors.New("invalid tools response lengths")
	}
	metaEnd, imageEnd := toolsResponseHeader+int(metadata), toolsResponseHeader+int(metadata+image)
	if !utf8.Valid(input[toolsResponseHeader:metaEnd]) || !utf8.Valid(input[metaEnd:imageEnd]) || !utf8.Valid(input[imageEnd:]) {
		return ToolsResponse{}, errors.New("invalid tools response UTF-8")
	}
	var response ToolsResponse
	decoder := json.NewDecoder(bytes.NewReader(input[toolsResponseHeader:metaEnd]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return ToolsResponse{}, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ToolsResponse{}, errors.New("trailing tools metadata")
	}
	if response.Format != ToolsFormat || response.Version != ToolsVersion || response.ImageJSON != "" || response.SymbolsJSON != "" {
		return ToolsResponse{}, errors.New("invalid tools response metadata")
	}
	response.ImageJSON, response.SymbolsJSON = string(input[metaEnd:imageEnd]), string(input[imageEnd:])
	return response, nil
}
