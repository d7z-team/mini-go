package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type schemaNode struct {
	Next     *schemaNode
	Children []schemaNode
	Lookup   map[string]*schemaNode
	Raw      json.RawMessage
	Ignored  map[string]any `json:"-"`
	private  chan int
}

func TestSchemaStructuresTraversesRecursiveFields(t *testing.T) {
	got, err := schemaStructures([]any{schemaNode{private: make(chan int)}, [2]*schemaNode{}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []reflect.Type{reflect.TypeOf(schemaNode{})}; !reflect.DeepEqual(got, want) {
		t.Fatalf("schema graph = %v, want %v", got, want)
	}
}

func TestSchemaStructuresRejectsUnsupportedFields(t *testing.T) {
	for _, root := range []any{struct{ Values map[int]string }{}, struct{ Callback func() }{}} {
		if _, err := schemaStructures([]any{root}); err == nil || !strings.Contains(err.Error(), "unsupported schema") {
			t.Fatalf("schema %T: %v", root, err)
		}
	}
}
