package bytecode

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestCanonicalTypeReferencesRetainIndependentTokens(t *testing.T) {
	var writer canonicalBuffer
	ref := types.TypeRef{Kind: types.Named, Named: types.TypeKey{ModulePath: "example/\xff<>&", DeclID: "T"}, Node: "node"}
	for i := 0; i < 3; i++ {
		if i == 1 {
			ref.Named.DeclID = "Changed"
		}
		if i == 2 {
			ref.Named.DeclID = "T"
		}
		var expected bytes.Buffer
		encoder := json.NewEncoder(&expected)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(ref); err != nil {
			t.Fatal(err)
		}
		start := len(writer.data)
		writer.encodeTypeRef(&ref)
		if !bytes.Equal(writer.data[start:], bytes.TrimSuffix(expected.Bytes(), []byte{'\n'})) {
			t.Fatalf("reference %d changed canonical encoding", i)
		}
		// Force growth between repeated references; retained tokens must not
		// depend on the destination backing allocation or later input mutation.
		writer.data = append(writer.data, make([]byte, 8192)...)
	}
}

func TestCanonicalStringsMatchJSONAcrossBufferReuse(t *testing.T) {
	var writer canonicalBuffer
	for round := 0; round < 3; round++ {
		for _, value := range []string{"module/example.T", "quoted\"\\\x00\u2028\u2029\xff", "中文/long-name"} {
			var expected bytes.Buffer
			encoder := json.NewEncoder(&expected)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(value); err != nil {
				t.Fatal(err)
			}
			writer.data = writer.data[:0]
			writer.string(value)
			if !bytes.Equal(writer.data, bytes.TrimSuffix(expected.Bytes(), []byte{'\n'})) {
				t.Fatalf("round %d: canonical token changed for %q", round, value)
			}
			writer.data = append(writer.data, make([]byte, 8192)...)
		}
	}
}

func TestFixedCanonicalEncodingMatchesJSON(t *testing.T) {
	values := []any{Artifact{}, ExecutionImage{}, ProgramSymbols{}, ConstPayload{}, LocalPayload{}, UpvaluePayload{}, GlobalPayload{}, AddressPayload{}, TypePayload{}, OperatorPayload{}, MakeSequencePayload{}, MakeMapPayload{}, MakeStructPayload{}, MakeSlicePayload{}, MakeWaitablePayload{}, SelectPayload{}, CountPayload{}, FieldPayload{}, ExportPayload{}, InitModulePayload{}, LabelPayload{}, JumpPayload{}, CallPayload{}, CallInterfacePayload{}, ClosurePayload{}, ReturnPayload{}, DeferPayload{}, CallFFIPayload{}, CallIntrinsicPayload{}}
	var populate func(reflect.Value, int)
	populate = func(value reflect.Value, depth int) {
		if depth > 12 {
			return
		}
		if value.Type() == reflect.TypeOf(json.RawMessage{}) {
			value.SetBytes([]byte(` {"number":9007199254740993,"text":"<>&\u2028"} `))
			return
		}
		switch value.Kind() {
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				if value.Field(i).CanSet() {
					populate(value.Field(i), depth+1)
				}
			}
		case reflect.Pointer:
			value.Set(reflect.New(value.Type().Elem()))
			populate(value.Elem(), depth+1)
		case reflect.Slice:
			value.Set(reflect.MakeSlice(value.Type(), 2, 2))
			for i := 0; i < 2; i++ {
				populate(value.Index(i), depth+1)
			}
		case reflect.Map:
			value.Set(reflect.MakeMap(value.Type()))
			for _, key := range []string{"z/中文<>&", "a/first"} {
				element := reflect.New(value.Type().Elem()).Elem()
				populate(element, depth+1)
				value.SetMapIndex(reflect.ValueOf(key), element)
			}
		case reflect.String:
			value.SetString("x\x00\t\n\r\b\f\"\\<>&\u2028\u2029中文\xff")
		case reflect.Bool:
			value.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			value.SetInt(-17)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			value.SetUint(19)
		}
	}
	for _, value := range values {
		for _, filled := range []bool{false, true} {
			v := reflect.New(reflect.TypeOf(value))
			if filled {
				populate(v.Elem(), 0)
			}
			for _, candidate := range []any{v.Interface(), v.Elem().Interface(), reflect.Zero(v.Type()).Interface()} {
				var expected bytes.Buffer
				encoder := json.NewEncoder(&expected)
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(candidate); err != nil {
					t.Fatal(err)
				}
				actual, err, ok := encodeFixedJSON(candidate)
				if err != nil || !ok || !bytes.Equal(actual, bytes.TrimSuffix(expected.Bytes(), []byte{'\n'})) {
					t.Fatalf("%T filled=%v: error=%v supported=%v\nactual: %s\nexpected: %s", candidate, filled, err, ok, actual, expected.Bytes())
				}
				decoded, reference := reflect.New(reflect.TypeOf(value)), reflect.New(reflect.TypeOf(value))
				if err := DecodeInstructionPayload(actual, decoded.Interface()); err != nil {
					t.Fatal(err)
				}
				if err := strictUnmarshal(actual, reference.Interface()); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded.Interface(), reference.Interface()) {
					t.Fatalf("%T decoding differs", candidate)
				}
			}
		}
	}
}

func TestFixedPayloadDecodingPreservesFieldMatchingAndMerging(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `{"local":"one","LOCAL":null,"rebind":true}`,
		`{"\u006cocal":"quoted","rebind":false}`, `{"local":"one","local":"two"}`,
		`{"local":12}`, `{"local":"one","unknown":true}`, `{"local":"one"} {}`,
		`{"type":{"node":"n"},"TYPE":{"primitive":2}}`, `{"type":{"unknown":1}}`,
		`{"result_count":1,"result_count":null}`, `{"result_count":1.5}`, `{"result_count":9223372036854775808}`,
	} {
		for _, initial := range []any{LocalPayload{Local: "initial", Rebind: true}, TypePayload{}, ReturnPayload{ResultCount: 7}} {
			actual, expected := reflect.New(reflect.TypeOf(initial)), reflect.New(reflect.TypeOf(initial))
			actual.Elem().Set(reflect.ValueOf(initial))
			expected.Elem().Set(reflect.ValueOf(initial))
			err := DecodeInstructionPayload([]byte(raw), actual.Interface())
			wantErr := strictUnmarshal([]byte(raw), expected.Interface())
			if (err == nil) != (wantErr == nil) {
				t.Fatalf("%T %s: error=%v want=%v", initial, raw, err, wantErr)
			}
			if err == nil && !reflect.DeepEqual(actual.Interface(), expected.Interface()) {
				t.Fatalf("%T %s: got %+v want %+v", initial, raw, actual.Interface(), expected.Interface())
			}
		}
	}
}

func TestCanonicalStringHandlesAllBytePairs(t *testing.T) {
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			value := string([]byte{byte(a), byte(b)})
			var expected bytes.Buffer
			encoder := json.NewEncoder(&expected)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(value); err != nil {
				t.Fatal(err)
			}
			var writer canonicalBuffer
			writer.string(value)
			if !bytes.Equal(writer.data, bytes.TrimSuffix(expected.Bytes(), []byte{'\n'})) {
				t.Fatalf("%x: got %s want %s", value, writer.data, expected.Bytes())
			}
		}
	}
}

func TestFixedCanonicalRejectsMalformedRawTokens(t *testing.T) {
	for _, raw := range []string{"", "{", "1 2"} {
		artifact := Artifact{Constants: []Constant{{Value: json.RawMessage(raw)}}}
		if _, err := CanonicalJSON(&artifact); err == nil {
			t.Fatalf("accepted raw token %q", raw)
		}
		image := ExecutionImage{Packages: map[string]PackageArchive{"main": {Artifact: json.RawMessage(raw)}}}
		if _, err := EncodeExecutionImage(&image); err == nil {
			t.Fatalf("accepted image raw token %q", raw)
		}
	}
}

func FuzzFixedPayloadDecodingMatchesStrictJSON(f *testing.F) {
	for _, seed := range []string{`{}`, `null`, `{"local":"a","LOCAL":null}`, `{"local":"a,}\\\"b","rebind":true}`, `{"unknown":1}`, `{"type":{"node":"n"},"type":{"primitive":2}}`, `{"cases":[{"channel":"c","send":"v"}]}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) == 0 || len(raw) > 64<<10 {
			return
		}
		for _, initial := range []any{LocalPayload{Local: "initial"}, TypePayload{}, SelectPayload{}, ReturnPayload{ResultCount: 7}} {
			actual, reference := reflect.New(reflect.TypeOf(initial)), reflect.New(reflect.TypeOf(initial))
			actual.Elem().Set(reflect.ValueOf(initial))
			reference.Elem().Set(reflect.ValueOf(initial))
			err := DecodeInstructionPayload(raw, actual.Interface())
			wantErr := strictUnmarshal(raw, reference.Interface())
			if (err == nil) != (wantErr == nil) || (err == nil && !reflect.DeepEqual(actual.Interface(), reference.Interface())) {
				t.Fatalf("%T %q: got %+v, %v; want %+v, %v", initial, raw, actual.Interface(), err, reference.Interface(), wantErr)
			}
			if json.Valid(raw) {
				validated := reflect.New(reflect.TypeOf(initial))
				validated.Elem().Set(reflect.ValueOf(initial))
				err := decodeInstructionPayload(raw, validated.Interface(), true)
				if (err == nil) != (wantErr == nil) || (err == nil && !reflect.DeepEqual(validated.Interface(), reference.Interface())) {
					t.Fatalf("validated %T %q: got %+v, %v; want %+v, %v", initial, raw, validated.Interface(), err, reference.Interface(), wantErr)
				}
			}
		}
	})
}
