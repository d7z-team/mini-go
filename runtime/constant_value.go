package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (m *moduleInstance) decodeConstant(typ any, raw json.RawMessage) (vmValue, error) {
	return decodeConstantAs(m, m.resolvedRuntimeType(typ), raw)
}

func decodeConstantAs(module *moduleInstance, runtimeType vmType, raw json.RawMessage) (vmValue, error) {
	typeText := runtimeType.String()
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if module.isSliceType(typeText) {
			return newVMValue(runtimeType, (*vmSlice)(nil)), nil
		}
		return newVMValue(runtimeType, nil), nil
	}
	kind := typeText
	if module != nil {
		if underlying, ok := module.underlyingType(kind); ok {
			kind = strings.TrimSpace(underlying)
		}
	}
	switch kind {
	case "Bool":
		var out bool
		if err := json.Unmarshal(raw, &out); err != nil {
			return vmValue{}, err
		}
		return newVMValue(runtimeType, out), nil
	case "String":
		var out string
		if err := json.Unmarshal(raw, &out); err != nil {
			return vmValue{}, err
		}
		return newVMValue(runtimeType, out), nil
	default:
		if module != nil && module.isByteSliceType(runtimeType) {
			var encoded string
			if err := json.Unmarshal(raw, &encoded); err != nil {
				return vmValue{}, err
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return vmValue{}, err
			}
			return newByteSliceHeaderValue(runtimeType, data, len(data), len(data)), nil
		}
		if out, ok, err := decodeNumericConstant(kind, raw); ok || err != nil {
			if err != nil {
				return vmValue{}, err
			}
			out.Type = runtimeType
			return out, nil
		}
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&number); err == nil {
			out, err := strconv.ParseInt(number.String(), 10, 64)
			if err != nil {
				return vmValue{}, err
			}
			return newVMValue(runtimeType, out), nil
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			return vmValue{}, err
		}
		return newVMValue(runtimeType, out), nil
	}
}

func truthy(value vmValue) (bool, error) {
	if out, ok := value.Data.(bool); ok {
		return out, nil
	}
	if value.Data == nil {
		return false, nil
	}
	return false, fmt.Errorf("expected Bool condition, got %s", value.Type)
}
