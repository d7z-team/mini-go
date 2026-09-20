package runtime

import (
	"slices"
	"strings"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
)

// structSchema is the immutable executable layout of a struct type. Values use
// field indexes internally; names remain available for reflection and display.
type structSchema struct {
	typ     vmType
	fields  []TypeFieldInfo
	indexes map[string]int
}

func newStructSchema(typ vmType, fields []TypeFieldInfo) *structSchema {
	schema := &structSchema{
		typ:     typ,
		fields:  append([]TypeFieldInfo(nil), fields...),
		indexes: make(map[string]int, len(fields)),
	}
	for index, field := range schema.fields {
		schema.indexes[field.Name] = index
	}
	return schema
}

func standaloneStructSchema(typ vmType) *structSchema {
	fields, ok := typ.StructFields()
	if !ok {
		return nil
	}
	info := make([]TypeFieldInfo, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		if name == "" || name == "_" {
			continue
		}
		info = append(info, TypeFieldInfo{
			Name: name, RuntimeType: vmType{Ref: field.Type, Table: typ.Table},
			Tag: field.Tag, Embedded: field.Embedded,
		})
	}
	return newStructSchema(typ, info)
}

// newRuntimeStructValue builds trusted runtime-owned payload structs. Normal
// guest construction uses newStructValue and resolves the declared schema.
func newRuntimeStructValue(module *moduleInstance, typ any, fields map[string]vmValue) vmValue {
	runtimeType := coerceRuntimeType(typ)
	var schema *structSchema
	if module != nil {
		runtimeType = module.resolvedRuntimeType(typ)
		schema, _ = module.structSchema(runtimeType)
	}
	if schema == nil {
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		slices.Sort(names)
		info := make([]TypeFieldInfo, 0, len(names))
		for _, name := range names {
			info = append(info, TypeFieldInfo{Name: name, RuntimeType: fields[name].Type})
		}
		schema = newStructSchema(runtimeType, info)
	}
	values := make([]vmValue, len(schema.fields))
	for name, value := range fields {
		if index, _, ok := schema.field(name); ok {
			values[index] = value
		}
	}
	return newVMValue(runtimeType, &vmStruct{schema: schema, values: values, sparse: true})
}

func setRuntimeStructField(value *vmStruct, name string, field vmValue) {
	if value == nil || value.schema == nil {
		return
	}
	value.mu.Lock()
	defer value.mu.Unlock()
	if index, _, ok := value.schema.field(name); ok && index < len(value.values) {
		value.values[index] = field
	}
}

func (schema *structSchema) field(name string) (int, TypeFieldInfo, bool) {
	if schema == nil {
		return 0, TypeFieldInfo{}, false
	}
	index, ok := schema.indexes[name]
	if !ok {
		return 0, TypeFieldInfo{}, false
	}
	return index, schema.fields[index], true
}

// vmStruct is a slot-based struct value. Uninitialized slots denote declared
// zero values and are attached to the value when first observed.
type vmStruct struct {
	mu     sync.Mutex
	schema *structSchema
	values []vmValue
	sparse bool
}

func (value *vmStruct) snapshot() ([]vmValue, bool) {
	value.mu.Lock()
	defer value.mu.Unlock()
	values := make([]vmValue, len(value.values), cap(value.values))
	copy(values, value.values)
	return values, value.sparse
}

func (value *vmStruct) fieldAt(index int) (vmValue, bool) {
	value.mu.Lock()
	defer value.mu.Unlock()
	if index < 0 || index >= len(value.values) || !value.values[index].Type.Valid() {
		return vmValue{}, false
	}
	return value.values[index], true
}

func (value *vmStruct) initializeField(index int, zero vmValue) vmValue {
	value.mu.Lock()
	defer value.mu.Unlock()
	if len(value.values) == 0 {
		value.values = make([]vmValue, len(value.schema.fields))
	}
	if !value.values[index].Type.Valid() {
		value.values[index] = zero
	}
	return value.values[index]
}

func structValueField(data any, field string) (vmValue, bool) {
	value, ok := data.(*vmStruct)
	if !ok || value == nil || value.schema == nil {
		return vmValue{}, false
	}
	index, _, ok := value.schema.field(field)
	if !ok {
		return vmValue{}, false
	}
	return value.fieldAt(index)
}

func (m *moduleInstance) structValueFieldInfo(object vmValue, field string) (TypeFieldInfo, bool) {
	value, ok := object.Data.(*vmStruct)
	if !ok || value == nil || value.schema == nil {
		return TypeFieldInfo{}, false
	}
	schema := value.schema
	if !schema.typ.Equal(object.Type) {
		if target, found := m.structSchema(object.Type); found {
			schema = target
		}
	}
	_, info, ok := schema.field(field)
	return info, ok
}

func isStructValue(data any) bool {
	value, ok := data.(*vmStruct)
	return ok && value != nil && value.schema != nil
}

func materializeStructValue(data any) (map[string]vmValue, bool) {
	value, ok := data.(*vmStruct)
	if !ok || value == nil || value.schema == nil {
		return nil, false
	}
	values, sparse := value.snapshot()
	fields := make(map[string]vmValue, len(values))
	for index, field := range value.schema.fields {
		if index < len(values) && values[index].Type.Valid() {
			fields[field.Name] = values[index]
		} else if !sparse {
			zero, ok := atomicZeroValue(field.RuntimeType)
			if !ok {
				zero = zeroVMValue(field.RuntimeType.String())
				zero.Type = field.RuntimeType
			}
			fields[field.Name] = zero
		}
	}
	return fields, true
}

func updatedStructValue(data any, field string, value vmValue) (any, bool) {
	current, ok := data.(*vmStruct)
	if !ok || current == nil || current.schema == nil {
		return nil, false
	}
	index, _, ok := current.schema.field(field)
	if !ok {
		return nil, false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if len(current.values) == 0 {
		current.values = make([]vmValue, len(current.schema.fields))
	}
	current.values[index] = value
	return current, true
}

func (m *moduleInstance) structSchema(typ any) (*structSchema, bool) {
	if m == nil {
		return nil, false
	}
	runtimeType := m.resolvedRuntimeType(typ)
	if runtimeType.ShapeKind() != types.Struct {
		return nil, false
	}
	key := runtimeType.String()
	if key == "" {
		return nil, false
	}
	if schema, ok := m.structSchemaCache.load(key); ok {
		return schema, schema != nil
	}

	fields, ok := m.moduleStructFieldInfo(runtimeType)
	if !ok {
		m.structSchemaCache.store(key, nil)
		return nil, false
	}
	schema := newStructSchema(runtimeType, fields)
	m.structSchemaCache.store(key, schema)
	return schema, true
}

type structFieldType struct {
	Name     string
	Type     string
	Tag      string
	Embedded bool
}

func parseStructFields(typ string) []structFieldType {
	runtimeType := coerceRuntimeType(typ)
	fields, ok := runtimeType.StructFields()
	if !ok || len(fields) == 0 {
		return nil
	}
	out := make([]structFieldType, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		if name == "" || name == "_" {
			continue
		}
		out = append(out, structFieldType{
			Name:     name,
			Type:     types.FormatWithTable(runtimeType.Table, field.Type),
			Tag:      field.Tag,
			Embedded: field.Embedded,
		})
	}
	return out
}

func formatCanonicalStructField(name, typ, tag string, embedded bool) string {
	prefix := ""
	if embedded {
		prefix = "embedded "
	}
	out := prefix + strings.TrimSpace(name) + ":" + strings.TrimSpace(typ)
	if tag != "" {
		out += " `" + tag + "`"
	}
	return out
}
