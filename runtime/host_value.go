package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type HostValueKind string

const (
	HostNilKind     HostValueKind = "nil"
	HostBoolKind    HostValueKind = "bool"
	HostIntKind     HostValueKind = "int"
	HostUintKind    HostValueKind = "uint"
	HostFloatKind   HostValueKind = "float"
	HostComplexKind HostValueKind = "complex"
	HostStringKind  HostValueKind = "string"
	HostBytesKind   HostValueKind = "bytes"
	HostArrayKind   HostValueKind = "array"
	HostSliceKind   HostValueKind = "slice"
	HostMapKind     HostValueKind = "map"
	HostStructKind  HostValueKind = "struct"
	HostAnyKind     HostValueKind = "any"
)

type HostValue struct {
	typ       string
	kind      HostValueKind
	boolValue bool
	intValue  int64
	uintValue uint64
	floatBits uint64
	realBits  uint64
	imagBits  uint64
	text      string
	bytes     []byte
	items     []HostValue
	entries   []HostMapEntry
	fields    []HostField
	dynamic   *HostValue
}

type RunResult struct {
	Values []HostValue
}

type HostMapEntry struct {
	Key   HostValue
	Value HostValue
}

type HostField struct {
	Name  string
	Value HostValue
}

func HostNil(typ string) HostValue { return HostValue{typ: typ, kind: HostNilKind} }
func HostBool(value bool) HostValue {
	return HostValue{typ: "Bool", kind: HostBoolKind, boolValue: value}
}

func HostInt(typ string, value int64) HostValue {
	return HostValue{typ: typ, kind: HostIntKind, intValue: value}
}

func HostUint(typ string, value uint64) HostValue {
	return HostValue{typ: typ, kind: HostUintKind, uintValue: value}
}

func HostFloat32(value float32) HostValue {
	return HostValue{typ: "Float32", kind: HostFloatKind, floatBits: uint64(math.Float32bits(value))}
}

func HostFloat64(value float64) HostValue {
	return HostValue{typ: "Float64", kind: HostFloatKind, floatBits: math.Float64bits(value)}
}

func HostComplex64(value complex64) HostValue {
	return HostValue{typ: "Complex64", kind: HostComplexKind, realBits: uint64(math.Float32bits(real(value))), imagBits: uint64(math.Float32bits(imag(value)))}
}

func HostComplex128(value complex128) HostValue {
	return HostValue{typ: "Complex128", kind: HostComplexKind, realBits: math.Float64bits(real(value)), imagBits: math.Float64bits(imag(value))}
}

func HostString(value string) HostValue {
	return HostValue{typ: "String", kind: HostStringKind, text: value}
}

func HostBytes(value []byte) HostValue {
	return HostValue{typ: "Slice<Uint8>", kind: HostBytesKind, bytes: append([]byte{}, value...)}
}

func HostArray(typ string, values ...HostValue) HostValue {
	return HostValue{typ: typ, kind: HostArrayKind, items: append([]HostValue(nil), values...)}
}

func HostSlice(typ string, values ...HostValue) HostValue {
	return HostValue{typ: typ, kind: HostSliceKind, items: append([]HostValue(nil), values...)}
}

func HostMap(typ string, entries ...HostMapEntry) HostValue {
	return HostValue{typ: typ, kind: HostMapKind, entries: append([]HostMapEntry(nil), entries...)}
}

func HostStruct(typ string, fields ...HostField) HostValue {
	return HostValue{typ: typ, kind: HostStructKind, fields: append([]HostField(nil), fields...)}
}

func HostAny(value HostValue) HostValue {
	cloned := value.clone()
	return HostValue{typ: "Any", kind: HostAnyKind, dynamic: &cloned}
}

func (v HostValue) Type() string           { return v.typ }
func (v HostValue) Kind() HostValueKind    { return v.kind }
func (v HostValue) Bool() (bool, bool)     { return v.boolValue, v.kind == HostBoolKind }
func (v HostValue) Int64() (int64, bool)   { return v.intValue, v.kind == HostIntKind }
func (v HostValue) Uint64() (uint64, bool) { return v.uintValue, v.kind == HostUintKind }
func (v HostValue) Float64() (float64, bool) {
	if v.kind != HostFloatKind {
		return 0, false
	}
	if v.typ == "Float32" {
		return float64(math.Float32frombits(uint32(v.floatBits))), true
	}
	return math.Float64frombits(v.floatBits), true
}

func (v HostValue) Complex128() (complex128, bool) {
	if v.kind != HostComplexKind {
		return 0, false
	}
	if v.typ == "Complex64" {
		return complex128(complex(math.Float32frombits(uint32(v.realBits)), math.Float32frombits(uint32(v.imagBits)))), true
	}
	return complex(math.Float64frombits(v.realBits), math.Float64frombits(v.imagBits)), true
}
func (v HostValue) StringValue() (string, bool) { return v.text, v.kind == HostStringKind }
func (v HostValue) Bytes() ([]byte, bool) {
	if v.kind != HostBytesKind {
		return nil, false
	}
	return append([]byte{}, v.bytes...), true
}

func (v HostValue) Items() ([]HostValue, bool) {
	if v.kind != HostArrayKind && v.kind != HostSliceKind {
		return nil, false
	}
	out := make([]HostValue, len(v.items))
	for i := range v.items {
		out[i] = v.items[i].clone()
	}
	return out, true
}

func (v HostValue) Entries() ([]HostMapEntry, bool) {
	if v.kind != HostMapKind {
		return nil, false
	}
	out := v.clone()
	return out.entries, true
}

func (v HostValue) Fields() ([]HostField, bool) {
	if v.kind != HostStructKind {
		return nil, false
	}
	out := v.clone()
	return out.fields, true
}

func (v HostValue) Dynamic() (HostValue, bool) {
	if v.kind != HostAnyKind || v.dynamic == nil {
		return HostValue{}, false
	}
	return v.dynamic.clone(), true
}

func (v HostValue) clone() HostValue {
	out := v
	if v.kind == HostBytesKind {
		out.bytes = append([]byte{}, v.bytes...)
	}
	out.items = make([]HostValue, len(v.items))
	for i := range v.items {
		out.items[i] = v.items[i].clone()
	}
	out.entries = make([]HostMapEntry, len(v.entries))
	for i := range v.entries {
		out.entries[i] = HostMapEntry{Key: v.entries[i].Key.clone(), Value: v.entries[i].Value.clone()}
	}
	out.fields = make([]HostField, len(v.fields))
	for i := range v.fields {
		out.fields[i] = HostField{Name: v.fields[i].Name, Value: v.fields[i].Value.clone()}
	}
	if v.dynamic != nil {
		dynamic := v.dynamic.clone()
		out.dynamic = &dynamic
	}
	return out
}

type hostValueMeasure struct {
	boundaryBytes int64
	logicalBytes  int64
	nodes         int
}

const maxMeasuredBytes = int64(^uint64(0) >> 1)

func addMeasuredBytes(total *int64, value int64) error {
	if value < 0 || *total > maxMeasuredBytes-value {
		return errors.New("host value size overflow")
	}
	*total += value
	return nil
}

func measureHostValue(ctx context.Context, value HostValue, limits Limits) error {
	if ctx == nil {
		ctx = context.Background()
	}
	measure := hostValueMeasure{}
	return measure.visit(ctx, value, limits, 0)
}

func (measure *hostValueMeasure) visit(ctx context.Context, value HostValue, limits Limits, depth int) error {
	measure.nodes++
	if measure.nodes&255 == 1 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if value.typ == "" {
		return errors.New("host value has no canonical type")
	}
	if depth > limits.MaxBoundaryDepth {
		return fmt.Errorf("host value depth limit exceeded: max %d", limits.MaxBoundaryDepth)
	}
	if err := addMeasuredBytes(&measure.boundaryBytes, int64(len(value.typ))); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.boundaryBytes, int64(len(value.text))); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.boundaryBytes, int64(len(value.bytes))); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.logicalBytes, ir.StorageRecordHeaderBytes); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.logicalBytes, int64(len(value.text))*ir.StorageByteBytes); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.logicalBytes, int64(len(value.bytes))*ir.StorageByteBytes); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.logicalBytes, int64(len(value.items)+len(value.fields))*ir.StorageDynamicBytes); err != nil {
		return err
	}
	if err := addMeasuredBytes(&measure.logicalBytes, int64(len(value.entries))*ir.StorageMapEntryBytes); err != nil {
		return err
	}
	if measure.boundaryBytes > limits.MaxBoundaryBytes {
		return fmt.Errorf("host value byte limit exceeded: max %d", limits.MaxBoundaryBytes)
	}
	switch value.kind {
	case HostNilKind, HostBoolKind, HostIntKind, HostUintKind, HostFloatKind, HostComplexKind, HostStringKind, HostBytesKind:
		return nil
	case HostArrayKind, HostSliceKind:
		for _, item := range value.items {
			if err := measure.visit(ctx, item, limits, depth+1); err != nil {
				return err
			}
		}
	case HostMapKind:
		for _, entry := range value.entries {
			if err := measure.visit(ctx, entry.Key, limits, depth+1); err != nil {
				return err
			}
			if err := measure.visit(ctx, entry.Value, limits, depth+1); err != nil {
				return err
			}
		}
	case HostStructKind:
		seen := make(map[string]struct{}, len(value.fields))
		for _, field := range value.fields {
			if field.Name == "" {
				return errors.New("host struct field has no name")
			}
			if _, ok := seen[field.Name]; ok {
				return fmt.Errorf("duplicate host struct field %q", field.Name)
			}
			seen[field.Name] = struct{}{}
			if err := addMeasuredBytes(&measure.boundaryBytes, int64(len(field.Name))); err != nil {
				return err
			}
			if err := addMeasuredBytes(&measure.logicalBytes, int64(len(field.Name))*ir.StorageByteBytes); err != nil {
				return err
			}
			if measure.boundaryBytes > limits.MaxBoundaryBytes {
				return fmt.Errorf("host value byte limit exceeded: max %d", limits.MaxBoundaryBytes)
			}
			if err := measure.visit(ctx, field.Value, limits, depth+1); err != nil {
				return err
			}
		}
	case HostAnyKind:
		if value.dynamic == nil {
			return errors.New("host Any value has no dynamic value")
		}
		return measure.visit(ctx, *value.dynamic, limits, depth+1)
	default:
		return fmt.Errorf("unknown host value kind %q", value.kind)
	}
	return nil
}
