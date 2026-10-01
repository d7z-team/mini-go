package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"

	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
)

func runToolsSchema(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("tools-schema", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("out", "spec/tools.json", "tools ABI schema output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("tools-schema accepts only -out")
	}
	definitions := map[string]any{}
	var schema func(reflect.Type) any
	schema = func(typ reflect.Type) any {
		switch typ.Kind() {
		case reflect.Pointer:
			return map[string]any{"anyOf": []any{schema(typ.Elem()), map[string]any{"type": "null"}}}
		case reflect.Struct:
			name := strings.ReplaceAll(typ.PkgPath()+"."+typ.Name(), "/", "_")
			if _, found := definitions[name]; !found {
				definitions[name] = nil
				properties := map[string]any{}
				for i := 0; i < typ.NumField(); i++ {
					field := typ.Field(i)
					if !field.IsExported() {
						continue
					}
					key := strings.Split(field.Tag.Get("json"), ",")[0]
					if key == "-" {
						continue
					}
					if key == "" {
						key = field.Name
					}
					properties[key] = schema(field.Type)
				}
				definitions[name] = map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
			}
			return map[string]any{"$ref": "#/$defs/" + name}
		case reflect.Slice:
			if typ.Elem().Kind() == reflect.Uint8 {
				return map[string]any{"type": []string{"string", "null"}, "contentEncoding": "base64"}
			}
			return map[string]any{"type": []string{"array", "null"}, "items": schema(typ.Elem())}
		case reflect.Map:
			return map[string]any{"type": []string{"object", "null"}, "additionalProperties": schema(typ.Elem())}
		case reflect.String:
			return map[string]any{"type": "string"}
		case reflect.Bool:
			return map[string]any{"type": "boolean"}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return map[string]any{"type": "integer"}
		case reflect.Float32, reflect.Float64:
			return map[string]any{"type": "number"}
		default:
			return map[string]any{}
		}
	}
	request := schema(reflect.TypeFor[compilerentry.ToolsRequest]())
	response := schema(reflect.TypeFor[compilerentry.ToolsResponse]())
	data, err := json.MarshalIndent(map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "title": "Mini-Go compiler tools ABI", "format": compilerentry.ToolsFormat, "version": compilerentry.ToolsVersion, "request": request, "response": response, "response_transport": map[string]any{"magic_hex": "4d4754330d0a1a0a", "header_bytes": 20, "lengths": "three u32 little-endian: metadata, image, symbols", "segments": []string{"metadata JSON with empty ImageJSON and SymbolsJSON", "raw UTF-8 ImageJSON", "raw UTF-8 SymbolsJSON"}, "max_bytes": compilerentry.MaxToolsInput}, "$defs": definitions}, "", "  ")
	if err != nil {
		return err
	}
	return writeGeneratedFile(*output, append(data, '\n'))
}
