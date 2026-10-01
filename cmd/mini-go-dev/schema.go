package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// schemaStructures collects the fixed JSON field graph shared by the identity
// and bytecode generators. Raw JSON remains an opaque leaf for each encoder.
func schemaStructures(roots []any) ([]reflect.Type, error) {
	seen := map[reflect.Type]bool{}
	var structures []reflect.Type
	var visit func(reflect.Type) error
	visit = func(t reflect.Type) error {
		if seen[t] || t == reflect.TypeOf(json.RawMessage{}) {
			return nil
		}
		seen[t] = true
		switch t.Kind() {
		case reflect.Struct:
			structures = append(structures, t)
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if !field.IsExported() || field.Tag.Get("json") == "-" {
					continue
				}
				if field.Anonymous {
					return fmt.Errorf("embedded JSON field %v.%s", t, field.Name)
				}
				if err := visit(field.Type); err != nil {
					return err
				}
			}
		case reflect.Pointer, reflect.Slice, reflect.Array:
			return visit(t.Elem())
		case reflect.Map:
			if t.Key().Kind() != reflect.String {
				return fmt.Errorf("unsupported schema map key %v", t.Key())
			}
			return visit(t.Elem())
		case reflect.Interface:
			if t.PkgPath() != "github.com/d7z-team/mini-go/runtime/bytecode" || t.Name() != "Payload" {
				return fmt.Errorf("unsupported schema interface %v", t)
			}
		case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			return fmt.Errorf("unsupported schema type %v", t)
		}
		return nil
	}
	for _, root := range roots {
		if err := visit(reflect.TypeOf(root)); err != nil {
			return nil, err
		}
	}
	sort.Slice(structures, func(i, j int) bool { return structures[i].String() < structures[j].String() })
	return structures, nil
}
