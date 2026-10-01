package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
)

func compareSnapshots(want, got snapshot) []difference {
	gotPackages := make(map[string]apiPackage, len(got.Packages))
	for _, pkg := range got.Packages {
		gotPackages[pkg.Path] = pkg
	}
	var out []difference
	for _, expected := range want.Packages {
		actual, present := gotPackages[expected.Path]
		if !present {
			out = append(out, difference{Package: expected.Path, Code: "missing_package"})
			continue
		}
		wantDecls, gotDecls := declarationMap(expected.Declarations), declarationMap(actual.Declarations)
		for key, methods := range requiredAPIs[expected.Path] {
			decl, found := gotDecls[key]
			if !found {
				out = append(out, difference{Package: expected.Path, Name: key, Code: "missing_required"})
				continue
			}
			for _, name := range methods {
				found := false
				for _, method := range decl.Methods {
					found = found || method.Name == name
				}
				if !found {
					out = append(out, difference{Package: expected.Path, Name: decl.Name + "." + name, Code: "missing_required_method"})
				}
			}
		}
		for key, declaration := range gotDecls {
			baseline, ok := wantDecls[key]
			if !ok {
				// Mini-Go exposes reference equality without exposing host addresses.
				if expected.Path == "reflect" && declarationMatches(apiDeclaration{Kind: "func", Name: "SameReference", Type: "function(reflect.Value, reflect.Value) Bool"}, declaration, true) {
					continue
				}
				out = append(out, difference{Package: expected.Path, Kind: declaration.Kind, Name: declaration.Name, Code: "extra"})
				continue
			}
			wantJSON, _ := json.Marshal(baseline)
			gotJSON, _ := json.Marshal(declaration)
			if !declarationMatches(baseline, declaration, expected.Complete) {
				out = append(out, difference{Package: expected.Path, Kind: declaration.Kind, Name: declaration.Name, Code: "mismatch", Want: string(wantJSON), Got: string(gotJSON)})
			}
		}
		if expected.Complete {
			for key, declaration := range wantDecls {
				if _, ok := gotDecls[key]; !ok {
					out = append(out, difference{Package: expected.Path, Kind: declaration.Kind, Name: declaration.Name, Code: "missing"})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i], out[j]
		return left.Package+"\x00"+left.Name+"\x00"+left.Kind+"\x00"+left.Code < right.Package+"\x00"+right.Name+"\x00"+right.Kind+"\x00"+right.Code
	})
	return out
}

var requiredAPIs = map[string]map[string][]string{
	"bytes":        {"func:CutLast": nil},
	"strings":      {"func:CutLast": nil},
	"net/url":      {"type:URL": {"Clone"}, "type:Values": {"Clone"}},
	"math/rand/v2": {"type:Rand": {"N"}},
}

func declarationMatches(expected, actual apiDeclaration, complete bool) bool {
	left, right := expected, actual
	left.Fields, right.Fields = nil, nil
	left.Methods, right.Methods = nil, nil
	left.TypeParams, right.TypeParams = nil, nil
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	if !bytes.Equal(leftJSON, rightJSON) {
		return false
	}
	if complete {
		expectedJSON, _ := json.Marshal(expected)
		actualJSON, _ := json.Marshal(actual)
		return bytes.Equal(expectedJSON, actualJSON)
	}
	return fieldsAreSubset(expected.Fields, actual.Fields) &&
		fieldsAreSubset(expected.Methods, actual.Methods) &&
		fieldsAreSubset(expected.TypeParams, actual.TypeParams)
}

func fieldsAreSubset(expected, actual []apiField) bool {
	available := make(map[string]apiField, len(expected))
	for _, field := range expected {
		available[field.Name] = field
	}
	for _, field := range actual {
		if want, ok := available[field.Name]; !ok || !reflect.DeepEqual(want, field) {
			return false
		}
	}
	return true
}

func declarationMap(values []apiDeclaration) map[string]apiDeclaration {
	out := make(map[string]apiDeclaration, len(values))
	for _, value := range values {
		out[value.Kind+":"+value.Name] = value
	}
	return out
}
