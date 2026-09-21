package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
	"github.com/d7z-team/mini-go/compiler/cache"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
)

var generatedCompilerCorpus = []struct {
	name string
	data []byte
}{
	{name: "generics", data: []byte{0, 3, 5, 8, 'g', 1}},
	{name: "methods", data: []byte{1, 13, 2, 21, 0, 2}},
	{name: "closures", data: []byte{2, 7, 11, 1}},
	{name: "collections", data: []byte{3, 9, 4, 6}},
	{name: "concurrency", data: []byte{4, 5, 3, 1}},
	{name: "conversions", data: []byte{5, 8, 2, 13}},
	{name: "embedding", data: []byte{14, 9, 4, 6, 0, 0xff}},
	{name: "globals", data: []byte{7, 12, 6, 2}},
}

func FuzzCompilerImageCheckMatchesNative(f *testing.F) {
	for _, seed := range generatedCompilerCorpus {
		f.Add(seed.data)
	}
	for _, seed := range [][]byte{
		{0, 3, 5, 8, 0, 1, 1},
		{16, 5, 3, 1, 0x80, 0xfe},
	} {
		f.Add(seed)
	}
	image := buildCompilerImage(f)
	program, err := minigoruntime.LoadExecutionImage(image)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64 {
			t.Skip()
		}
		// Keep each fuzz callback bounded; full Prepare comparisons run in the fixed differential corpus.
		request := generatedCompilerRequest(data, compilerentry.OperationCheck)
		native := compilerentry.NewService(cache.TransientConfig{MaxEntries: 32, MaxBytes: 96 << 20})
		want := native.Execute(request)
		native.Close()

		instance, err := program.Instantiate(context.Background(), compilerImageInstanceOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := instance.Close(); err != nil {
				t.Error(err)
			}
		}()
		input, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		got := callCompilerImage(t, instance, input)
		compareCompilerResponse(t, got, want)
	})
}

func generatedCompilerRequest(data []byte, operation compilerentry.Operation) compilerentry.Request {
	byteAt := func(index int) byte {
		if index >= len(data) {
			return 0
		}
		return data[index]
	}
	integer := func(index int) int { return int(byteAt(index)%41) - 20 }
	a, b, c := integer(1), integer(2), integer(3)
	tags := []string(nil)
	if byteAt(0)&8 != 0 {
		tags = []string{"feature"}
	}

	invalid := ""
	switch byteAt(0) / 16 % 5 {
	case 1:
		invalid = "func Invalid() int { return missing }\n"
	case 2:
		invalid = "var Invalid int = \"value\"\n"
	case 3:
		invalid = "type Invalid map[[]int]int\n"
	case 4:
		invalid = "func Invalid[T int | int](value T) T { return value }\n"
	}
	resource := append([]byte(nil), data...)
	if len(resource) == 0 {
		resource = []byte("minigo")
	}

	var librarySource, mainSource string
	extraSource := "package main\n"
	switch byteAt(0) % 8 {
	case 0:
		librarySource = `package library

func Identity[T any](value T) T { return value }
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

func Result() int {
	return library.Identity[int](%d) + library.Identity[int](%d)
}

	%s`, a, b, invalid)
		if byteAt(6)&1 != 0 {
			librarySource += "\nfunc Value() int { return 7 }\nfunc Wrap[T any](v T) []T { return []T{v} }\n"
			mainSource = fmt.Sprintf("package main\nimport . \"fuzz/library\"\nfunc Result() int { var rows = Wrap([]int{%d}); return Identity(rows[0])[0] + Local(%d) }\n%s", a, b, invalid)
			extraSource = "package main\nimport . \"fuzz/library\"\nfunc Local[T any](v T) int { return Value() }\n"
		}
	case 1:
		librarySource = `package library

type Pair struct { Left int; Right int }
func (pair Pair) Total() int { return pair.Left + pair.Right }
type Totaler interface { Total() int }
func Total(value Totaler) int { return value.Total() }
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

func Result() int {
	pair := library.Pair{Left: %d, Right: %d}
	return library.Total(pair)
}

%s`, a, b, invalid)
	case 2:
		librarySource = `package library

func Apply(function func(int) int, value int) int { return function(value) }
func Variadic(base int, values ...int) int {
	total := base
	for _, value := range values { total += value }
	return total
}
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

func Result() int {
	offset := %d
	closure := func(value int) int { return value + offset }
	return library.Apply(closure, %d) + library.Variadic(0, %d, %d)
}

%s`, a, b, a, c, invalid)
	case 3:
		librarySource = `package library

func Fold(values []int) int {
	total := 0
	for index, value := range values {
		if index%2 == 0 { total += value } else { total -= value }
	}
	return total
}
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

func Result() int {
	array := [3]int{%d, %d, %d}
	values := array[:]
	lookup := map[string]int{"first": values[0], "last": values[2]}
	total := library.Fold(values)
	switch lookup["first"] + lookup["last"] {
	case 0:
		return total
	default:
		return total + len(values)
	}
}

%s`, a, b, c, invalid)
	case 4:
		librarySource = `package library

func Guard(value int) (result int) {
	result = value
	defer func() {
		if recover() != nil { result = -value }
	}()
	if value < 0 { panic(value) }
	return
}
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

func receive(value int) int {
	channel := make(chan int, 1)
	go func() { channel <- value }()
	select {
	case received := <-channel:
		return received
	default:
		return 0
	}
}

func Result() int {
	defer func() {}()
	return library.Guard(%d) + receive(%d)
}

%s`, a, b, invalid)
	case 5:
		librarySource = `package library

func Kind(value any) int {
	switch current := value.(type) {
	case int:
		return current
	case string:
		return len(current)
	default:
		return 0
	}
}

func Numeric(value int) int {
	small := int8(value)
	wide := uint16(value)
	fractional := float32(small)
	number := complex(fractional, float32(1))
	return int(real(number)) + int(wide)
}
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

func Result() int {
	return library.Kind(%d) + library.Kind("世界") + library.Numeric(%d)
}

%s`, a, b, invalid)
	case 6:
		librarySource = `package library

func Value(value int) int { return value }
`
		mainSource = fmt.Sprintf(`package main

import (
	_ "embed"
	"fuzz/library"
)

//go:embed asset.bin
var asset string

func Result() int {
	return library.Value(%d) + selected() + len(asset)
}

%s`, a, invalid)
	default:
		librarySource = `package library

type Number int
var Value int
var Numbers [2]Number
func Bump(value *int) { *value = *value + 1 }
`
		mainSource = fmt.Sprintf(`package main

import "fuzz/library"

const Third = 1.0 / 3.0

func Result() int {
	library.Value = %d
	library.Bump(&library.Value)
	library.Numbers = [2]library.Number{library.Number(%d), library.Number(%d)}
	last := &library.Numbers[1]
	*last += 1
	return library.Value + int(library.Numbers[0]) + int(*last) + int(Third*3)
}

%s`, a, b, c, invalid)
	}

	request := compilerentry.Request{
		Format: compilerentry.ServiceFormat, Version: compilerentry.ServiceVersion,
		Operation: operation, Root: "fuzz/app", Tags: tags,
		Packages: []compilerentry.Package{
			{
				Namespace: "module:fuzz", PackagePath: "library", ModulePath: "fuzz/library",
				Files: []compilerentry.File{{Path: "library.mgo", Text: librarySource}},
			},
			{
				Namespace: "module:fuzz", PackagePath: "app", ModulePath: "fuzz/app",
				Files: []compilerentry.File{
					{Path: "main.mgo", Text: mainSource},
					{Path: "generic.mgo", Text: extraSource},
					{Path: "mode_default.mgo", Text: "//go:build !feature\n\npackage main\nfunc selected() int { return 3 }\n"},
					{Path: "mode_feature.mgo", Text: "//go:build feature\n\npackage main\nfunc selected() int { return 7 }\n"},
				},
				Resources: []compilerentry.Resource{{Path: "asset.bin", Data: resource}},
			},
		},
	}
	if operation == compilerentry.OperationPrepare {
		request.Optimization = compiler.OptimizationLevel(byteAt(4) % 3)
		request.Symbols = byteAt(5)%2 != 0
		request.EntryPoints = []compiler.EntryPoint{{Name: "result", ModulePath: "fuzz/app", Function: "Result"}}
	}
	return request
}
