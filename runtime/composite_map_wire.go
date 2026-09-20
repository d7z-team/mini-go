package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

type mapKeyWire struct {
	Type    string            `json:"type"`
	Bool    *bool             `json:"bool,omitempty"`
	String  *string           `json:"string,omitempty"`
	Int64   *int64            `json:"int64,omitempty"`
	Uint64  *uint64           `json:"uint64,omitempty"`
	Float   *string           `json:"float,omitempty"`
	Complex *string           `json:"complex,omitempty"`
	Ref     *string           `json:"ref,omitempty"`
	Array   []mapKeyWire      `json:"array,omitempty"`
	Struct  []mapKeyWireField `json:"struct,omitempty"`

	NonReflexive bool `json:"-"`
}

type mapKeyWireField struct {
	Name  string     `json:"name"`
	Value mapKeyWire `json:"value"`
}

type mapReferenceKeyWire struct {
	Type     string `json:"type"`
	Identity string `json:"identity"`
}

func (m *moduleInstance) mapKey(value vmValue) (vmMapKey, error) {
	value, present, err := m.unwrapComparableDynamicValue(value)
	if err != nil {
		return vmMapKey{}, err
	}
	if !present {
		return vmMapKey{Kind: vmMapKeyGeneric, Text: "nil-interface"}, nil
	}
	typeIdentity := m.runtimeTypeIdentity(value.Type)
	switch data := value.materializedData().(type) {
	case bool:
		return vmMapKey{Kind: vmMapKeyBool, TypeIdentity: typeIdentity, Bool: data}, nil
	case string:
		return vmMapKey{Kind: vmMapKeyString, TypeIdentity: typeIdentity, Text: data}, nil
	case int64:
		return vmMapKey{Kind: vmMapKeyInt, TypeIdentity: typeIdentity, Int64: data}, nil
	case uint64:
		return vmMapKey{Kind: vmMapKeyUint, TypeIdentity: typeIdentity, Uint64: data}, nil
	}
	if m.isReferenceComparableType(value.Type) {
		identity, err := referenceComparableMapKeyIdentity(value.Data)
		if err != nil {
			return vmMapKey{}, err
		}
		data, err := json.Marshal(mapReferenceKeyWire{
			Type:     m.runtimeTypeIdentity(value.Type),
			Identity: identity,
		})
		if err != nil {
			return vmMapKey{}, err
		}
		return vmMapKey{Kind: vmMapKeyGeneric, Text: "r:" + string(data)}, nil
	}
	value = m.cloneValueForStore(value)
	wire, err := m.mapKeyWire(value, map[string]struct{}{})
	if err != nil {
		return vmMapKey{}, err
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return vmMapKey{}, err
	}
	return vmMapKey{Kind: vmMapKeyGeneric, Text: "v:" + string(data), NonReflexive: wire.NonReflexive}, nil
}

func (m *moduleInstance) mapKeyWire(value vmValue, seen map[string]struct{}) (mapKeyWire, error) {
	value, present, err := m.unwrapComparableDynamicValue(value)
	if err != nil {
		return mapKeyWire{}, err
	}
	if !present {
		return mapKeyWire{Type: "nil-interface"}, nil
	}
	ok, err := m.isComparableRuntimeType(value.Type, map[string]struct{}{})
	if err != nil {
		return mapKeyWire{}, err
	}
	if !ok {
		return mapKeyWire{}, newGuestPanic(fmt.Errorf("unsupported map key type %s", value.Type))
	}
	wire := mapKeyWire{Type: m.runtimeTypeIdentity(value.Type)}
	if m.isReferenceComparableType(value.Type) {
		identity, err := referenceComparableMapKeyIdentity(value.Data)
		if err != nil {
			return mapKeyWire{}, err
		}
		wire.Ref = &identity
		return wire, nil
	}
	switch data := value.materializedData().(type) {
	case bool:
		wire.Bool = &data
		return wire, nil
	case string:
		wire.String = &data
		return wire, nil
	case int64:
		wire.Int64 = &data
		return wire, nil
	case uint64:
		wire.Uint64 = &data
		return wire, nil
	case float64:
		if data == 0 {
			data = 0
		}
		text := strconv.FormatUint(math.Float64bits(data), 16)
		wire.Float = &text
		wire.NonReflexive = math.IsNaN(data)
		return wire, nil
	case complex128:
		realPart, imaginaryPart := real(data), imag(data)
		if realPart == 0 {
			realPart = 0
		}
		if imaginaryPart == 0 {
			imaginaryPart = 0
		}
		text := strconv.FormatUint(math.Float64bits(realPart), 16) + ":" + strconv.FormatUint(math.Float64bits(imaginaryPart), 16)
		wire.Complex = &text
		wire.NonReflexive = math.IsNaN(realPart) || math.IsNaN(imaginaryPart)
		return wire, nil
	}
	if _, _, ok := m.arrayType(value.Type); ok {
		array, ok := value.Data.(*vmArray)
		if !ok {
			return mapKeyWire{}, fmt.Errorf("invalid array map key %s", value.Type)
		}
		items := array.values()
		wire.Array = make([]mapKeyWire, 0, len(items))
		for i, item := range items {
			itemWire, err := m.mapKeyWire(item, seen)
			if err != nil {
				return mapKeyWire{}, fmt.Errorf("array key element %d: %w", i, err)
			}
			wire.NonReflexive = wire.NonReflexive || itemWire.NonReflexive
			wire.Array = append(wire.Array, itemWire)
		}
		return wire, nil
	}
	if fields, ok := m.comparableStructFields(value.Type); ok {
		identity := m.runtimeTypeIdentity(value.Type)
		if _, recursive := seen[identity]; recursive {
			return mapKeyWire{}, fmt.Errorf("recursive map key type %s", value.Type)
		}
		seen[identity] = struct{}{}
		defer delete(seen, identity)
		if _, ok := value.Data.(*vmStruct); !ok {
			return mapKeyWire{}, fmt.Errorf("invalid struct map key %s", value.Type)
		}
		wire.Struct = make([]mapKeyWireField, 0, len(fields))
		for _, field := range fields {
			fieldValue, err := loadFieldValue(m, value, field.Name)
			if err != nil {
				return mapKeyWire{}, fmt.Errorf("struct key field %q: %w", field.Name, err)
			}
			fieldWire, err := m.mapKeyWire(fieldValue, seen)
			if err != nil {
				return mapKeyWire{}, fmt.Errorf("struct key field %q: %w", field.Name, err)
			}
			wire.NonReflexive = wire.NonReflexive || fieldWire.NonReflexive
			wire.Struct = append(wire.Struct, mapKeyWireField{Name: field.Name, Value: fieldWire})
		}
		return wire, nil
	}
	return mapKeyWire{}, fmt.Errorf("unsupported map key type %s", value.Type)
}
