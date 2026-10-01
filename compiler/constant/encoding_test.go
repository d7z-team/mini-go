package constant

import (
	"bytes"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestLiteralEncodingPreservesTypeAndBytes(t *testing.T) {
	for _, test := range []struct {
		value   *Value
		typ     string
		untyped bool
		want    string
	}{
		{Scalar("null"), "Slice<Int>", false, "null"},
		{Scalar("-0"), "Float64", false, "-0"},
		{Scalar("18446744073709551615"), "Uint64", false, `"18446744073709551615"`},
		{Scalar("1"), "Float64", true, `"1/1"`},
		{Scalar("1/8"), "Float64", false, "0.125"},
		{String("\xff\x00", "String", false).Ref(), "String", false, `{"bytes":"/wA="}`},
		{String("\xff\x00", "String", false).Ref(), "Slice<Uint8>", false, `"/wA="`},
		{String("true", "String", true).Ref(), "String", true, `"true"`},
		{&Value{Real: "1/2", Imag: "-1/4"}, "Complex128", false, `{"real":0.5,"imag":-0.25}`},
	} {
		table := &types.TypeTable{}
		parser := types.NewParser("example", table)
		ref, err := parser.Parse(test.typ)
		if err != nil {
			t.Fatal(err)
		}
		before := *test.value
		got := test.value.Encode(table, ref, test.untyped)
		if string(got) != test.want {
			t.Fatalf("%s: %s, want %s", test.typ, got, test.want)
		}
		if *test.value != before {
			t.Fatal("encoding mutated a shared literal")
		}
	}
}

func TestImportedConstantsPreserveExactValues(t *testing.T) {
	for _, test := range []struct {
		raw, typ, real, imaginary string
	}{
		{`18446744073709551615`, "Uint64", "18446744073709551615", ""},
		{`"18446744073709551615"`, "Uint64", "18446744073709551615", ""},
		{`"1/3"`, "Float64", "1/3", ""},
		{`{"real":0.5,"imag":-0.25}`, "Complex128", "1/2", "-1/4"},
		{`{"real":"1/3","imag":"2/7"}`, "Complex128", "1/3", "2/7"},
	} {
		value, ok := FromJSON([]byte(test.raw), test.typ, true)
		if !ok {
			t.Fatalf("decode %s", test.raw)
		}
		realPart := value.Text
		if test.imaginary != "" {
			realPart = value.Real
		}
		if realPart != test.real || value.Imag != test.imaginary {
			t.Fatalf("%s: got %+v", test.raw, value)
		}
	}
}

func TestStringFactsPreserveArbitraryBytesThroughCopies(t *testing.T) {
	data := bytes.Repeat([]byte{0, 0xff, '"', '\\', '\n', 0x80}, 8192)
	value := String(string(data), "String", true)
	literal := value.Ref()
	if literal.Kind() != StringValue || literal.Text != string(data) {
		t.Fatal("literal bytes changed")
	}
	decoded, ok := FromJSON(literal.JSON(), "String", true)
	if !ok || decoded.Text != string(data) || !decoded.IsString {
		t.Fatal("wire round trip changed bytes")
	}
	literal.Type = "example.Named"
	if value.Type != "String" {
		t.Fatal("literal metadata aliases its source")
	}
}

func TestByteSliceEncodingPreservesNamedElementType(t *testing.T) {
	table := &types.TypeTable{}
	if err := table.Add(types.TypeNode{
		ID: "decl.example.Octet", Kind: types.Named,
		Identity:   types.TypeKey{ModulePath: "example", DeclID: "Octet"},
		Underlying: types.Builtin(types.PrimitiveUint8),
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := types.NewParser("example", table).Parse("Slice<example.Octet>")
	if err != nil {
		t.Fatal(err)
	}
	value := String("\x00\xff", "String", false)
	if got := string(value.Encode(table, ref, false)); got != `"AP8="` {
		t.Fatalf("named byte slice encoding = %s", got)
	}
}

func BenchmarkConstantStringBoundary(b *testing.B) {
	value := String(string(bytes.Repeat([]byte{0, 255, 128, 1}, 16384)), "String", true)
	b.ReportAllocs()
	for b.Loop() {
		_ = value.Encode(nil, types.Builtin(types.PrimitiveString), true)
	}
}
