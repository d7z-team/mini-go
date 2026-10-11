package bytecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

func strictUnmarshal(raw json.RawMessage, out any) error {
	if err := validateStrictJSON(raw, reflect.TypeOf(out)); err != nil {
		return err
	}
	return unmarshalValidatedJSON(raw, out)
}

// DecodeWireJSON decodes one current bytecode contract value. It rejects
// trailing input, duplicate or unknown fields, folded field names, and null
// values for non-nullable fields.
func DecodeWireJSON(raw []byte, out any) error {
	return strictUnmarshal(raw, out)
}

func unmarshalValidatedJSON(raw json.RawMessage, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// validateStrictJSON applies the current wire schema before encoding/json can
// fold field names, overwrite duplicate keys, or silently ignore null scalars.
func validateStrictJSON(raw []byte, target reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	for target != nil && target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if err := validateJSONToken(decoder, token, target, "$", false); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateJSONToken(decoder *json.Decoder, token json.Token, target reflect.Type, path string, nullable bool) error {
	if target != nil {
		if target == rawMessageType || target.Kind() == reflect.Interface {
			target = nil
		} else {
			for target.Kind() == reflect.Pointer {
				nullable = true
				target = target.Elem()
			}
			if target.Kind() == reflect.Slice || target.Kind() == reflect.Map {
				nullable = true
			}
		}
	}
	if token == nil {
		if target == nil || nullable {
			return nil
		}
		return fmt.Errorf("json: null is not valid for %s at %s", target, path)
	}

	delimiter, compound := token.(json.Delim)
	if !compound {
		if target == nil {
			return nil
		}
		kind := target.Kind()
		valid := false
		switch token.(type) {
		case string:
			valid = kind == reflect.String
		case bool:
			valid = kind == reflect.Bool
		case json.Number:
			valid = kind >= reflect.Int && kind <= reflect.Float64 || kind >= reflect.Uint && kind <= reflect.Uintptr
		}
		if !valid {
			return fmt.Errorf("json: invalid value for %s at %s", target, path)
		}
		return nil
	}

	switch delimiter {
	case '{':
		var fields map[string]reflect.Type
		var element reflect.Type
		if target != nil {
			switch target.Kind() {
			case reflect.Struct:
				fields = make(map[string]reflect.Type, target.NumField())
				for index := range target.NumField() {
					field := target.Field(index)
					if !field.IsExported() {
						continue
					}
					name := strings.Split(field.Tag.Get("json"), ",")[0]
					if name == "-" {
						continue
					}
					if name == "" {
						name = field.Name
					}
					fields[name] = field.Type
				}
			case reflect.Map:
				if target.Key().Kind() != reflect.String {
					return fmt.Errorf("json: unsupported map key %s at %s", target.Key(), path)
				}
				element = target.Elem()
			default:
				return fmt.Errorf("json: object is not valid for %s at %s", target, path)
			}
		}
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("json: invalid object key at %s", path)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("json: duplicate field %q at %s", key, path)
			}
			seen[key] = struct{}{}
			fieldType := element
			if fields != nil {
				var exists bool
				fieldType, exists = fields[key]
				if !exists {
					return fmt.Errorf("json: unknown field %q at %s", key, path)
				}
			}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := validateJSONToken(decoder, value, fieldType, path+"."+key, false); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		if target != nil && target.Kind() != reflect.Slice && target.Kind() != reflect.Array {
			// Tuple-shaped models use custom UnmarshalJSON methods. Their decoder
			// owns element types and exact length; this pass still validates every
			// nested object and duplicate key.
			target = nil
		}
		var element reflect.Type
		expectedLength := -1
		if target != nil {
			element = target.Elem()
			if target.Kind() == reflect.Array {
				expectedLength = target.Len()
			}
		}
		length := 0
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := validateJSONToken(decoder, value, element, fmt.Sprintf("%s[%d]", path, length), false); err != nil {
				return err
			}
			length++
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
		if expectedLength >= 0 && length != expectedLength {
			return fmt.Errorf("json: array at %s has length %d, want %d", path, length, expectedLength)
		}
		return nil
	default:
		return fmt.Errorf("json: unexpected delimiter %q at %s", delimiter, path)
	}
}

// DecodeInstructionPayload decodes one instruction payload using the current
// strict field, null, duplicate-key, integer-range and framing rules.
func DecodeInstructionPayload(raw json.RawMessage, out any) error {
	return decodeInstructionPayload(raw, out, false)
}

func decodeInstructionPayload(raw json.RawMessage, out any, validJSON bool) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
		validJSON = true
	}
	if err := validateStrictJSON(raw, reflect.TypeOf(out)); err != nil {
		return err
	}
	if validJSON || json.Valid(raw) {
		if handled, err := decodeFixedPayload(raw, out); handled && err == nil {
			return nil
		}
	}
	return unmarshalValidatedJSON(raw, out)
}
