package runtime

import (
	"encoding/json"
	"hash/maphash"
	"strings"
	"sync"

	"github.com/d7z-team/mini-go/compiler/types"
)

type runtimeNumericEntry struct {
	info types.NumericInfo
	ok   bool
}

var runtimeNumericByPrimitive = func() [types.PrimitiveFunction + 1]runtimeNumericEntry {
	var entries [types.PrimitiveFunction + 1]runtimeNumericEntry
	for primitive := types.PrimitiveInvalid; primitive <= types.PrimitiveFunction; primitive++ {
		entries[primitive].info, entries[primitive].ok = types.NumericTypeInfo(nil, types.TypeRef{
			Kind:      types.Primitive,
			Primitive: primitive,
		})
	}
	return entries
}()

var predeclaredRuntimeTypes = func() map[string]vmType {
	values := map[string]vmType{
		"Void": {Ref: types.VoidType(), text: "Void"},
		"Any":  {Ref: types.AnyType(), text: "Any"},
	}
	for primitive := types.PrimitiveBool; primitive <= types.PrimitiveFunction; primitive++ {
		name := types.PrimitiveName(primitive)
		values[name] = vmType{Ref: types.Builtin(primitive), text: name}
	}
	return values
}()

var boolRuntimeType = predeclaredRuntimeTypes["Bool"]

const (
	parsedRuntimeTypeCacheSlots = 256
	maxCachedRuntimeTypeLength  = 1024
)

type parsedRuntimeTypeEntry struct {
	text string
	typ  vmType
}

var parsedRuntimeTypeCache = struct {
	sync.RWMutex
	seed    maphash.Seed
	entries [parsedRuntimeTypeCacheSlots]parsedRuntimeTypeEntry
}{seed: maphash.MakeSeed()}

func runtimeTypeCacheIndex(text string) int {
	return int(maphash.String(parsedRuntimeTypeCache.seed, text) % parsedRuntimeTypeCacheSlots)
}

func runtimeTypeFromText(text string) vmType {
	text = strings.TrimSpace(text)
	if text == "" {
		return vmType{}
	}
	if runtimeType, ok := predeclaredRuntimeTypes[text]; ok {
		return runtimeType
	}
	cacheIndex := -1
	if len(text) <= maxCachedRuntimeTypeLength {
		cacheIndex = runtimeTypeCacheIndex(text)
		parsedRuntimeTypeCache.RLock()
		entry := parsedRuntimeTypeCache.entries[cacheIndex]
		parsedRuntimeTypeCache.RUnlock()
		if entry.text == text {
			return entry.typ
		}
	}
	table := &types.TypeTable{}
	parser := types.NewParser("runtime", table)
	ref, err := parser.Parse(text)
	var runtimeType vmType
	if err != nil {
		runtimeType = namedRuntimeTypeRef("runtime.invalid", text)
		runtimeType.text = text
	} else {
		runtimeType = runtimeTypeWithTable(ref, table)
		runtimeType.text = types.FormatWithTable(table, ref)
	}
	runtimeType.standalone = true
	if cacheIndex < 0 {
		return runtimeType
	}
	parsedRuntimeTypeCache.Lock()
	entry := parsedRuntimeTypeCache.entries[cacheIndex]
	if entry.text == text {
		runtimeType = entry.typ
	} else {
		parsedRuntimeTypeCache.entries[cacheIndex] = parsedRuntimeTypeEntry{text: text, typ: runtimeType}
	}
	parsedRuntimeTypeCache.Unlock()
	return runtimeType
}

func coerceRuntimeType(value any) vmType {
	switch value := value.(type) {
	case vmType:
		return value
	case types.TypeRef:
		return vmType{Ref: value}
	case string:
		if runtimeType, ok := predeclaredRuntimeTypes[value]; ok {
			return runtimeType
		}
		return runtimeTypeFromText(value)
	default:
		return vmType{}
	}
}

// vmType is the executable view of an artifact TypeRef. The table is
// retained with the reference so structural nodes remain resolvable after a
// value crosses a function or module boundary.
type vmType struct {
	Ref           types.TypeRef
	Table         *types.TypeTable
	text          string
	underlyingRef types.TypeRef
	hasUnderlying bool
	standalone    bool
}

func runtimeTypeWithTable(ref types.TypeRef, table *types.TypeTable) vmType {
	runtimeType := vmType{Ref: ref, Table: table}
	if table != nil && ref.Kind == types.Named {
		underlying := table.Underlying(ref)
		if underlying.Valid() && underlying != ref {
			runtimeType.underlyingRef = underlying
			runtimeType.hasUnderlying = true
		}
	}
	return runtimeType
}

// MarshalJSON is the execution-result presentation boundary. Artifact type
// metadata remains structured; CLI result JSON uses the stable readable type
// spelling.
func (t vmType) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

func (t vmType) Valid() bool {
	return t.Ref.Valid()
}

func (t vmType) String() string {
	if t.text != "" {
		return t.text
	}
	if !t.Ref.Valid() {
		return ""
	}
	return types.FormatWithTable(t.Table, t.Ref)
}

func (t vmType) Equal(other vmType) bool {
	if t.standalone && other.standalone {
		return t.String() == other.String()
	}
	if t.Ref == other.Ref {
		if t.Ref.Node == "" || t.Table == other.Table {
			return true
		}
		return false
	}
	if t.Ref.Kind != types.Named || other.Ref.Kind != types.Named {
		return false
	}
	return canonicalRuntimeTypeRef(t.Ref) == canonicalRuntimeTypeRef(other.Ref)
}

func (t vmType) derived(ref types.TypeRef) vmType {
	derived := vmType{Ref: ref, Table: t.Table, standalone: t.standalone}
	if t.standalone {
		derived.text = types.FormatWithTable(t.Table, ref)
	}
	return derived
}

func (t vmType) Is(kind types.Kind) bool {
	if t.Ref.Kind == kind {
		return true
	}
	if t.Table == nil || t.Ref.Kind != types.Named {
		return false
	}
	return t.Table.Underlying(t.Ref).Kind == kind
}

func (t vmType) Primitive(kind types.PrimitiveKind) bool {
	ref := t.Ref
	if t.Table != nil && ref.Kind == types.Named {
		ref = t.Table.Underlying(ref)
	}
	return ref.Kind == types.Primitive && ref.Primitive == kind
}

func (t vmType) NumericInfo() (types.NumericInfo, bool) {
	if t.Ref.Kind == types.Primitive && t.Ref.Primitive <= types.PrimitiveFunction {
		entry := runtimeNumericByPrimitive[t.Ref.Primitive]
		return entry.info, entry.ok
	}
	if t.Ref.Kind != types.Named {
		return types.NumericInfo{}, false
	}
	return types.NumericTypeInfo(t.Table, t.Ref)
}

type runtimeWaitableInfo struct {
	Direction types.ChannelDir
	Elem      vmType
}

type runtimeFunctionInfo struct {
	Signature types.FunctionSignature
}

type runtimeInterfaceInfo struct {
	Methods []types.Method
	Terms   []types.TypeTerm
	TypeSet bool
}

func (t vmType) WaitableInfo() (runtimeWaitableInfo, bool) {
	underlying := t.Underlying()
	if underlying.Ref.Kind != types.Waitable {
		return runtimeWaitableInfo{}, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != types.Waitable || !node.Elem.Valid() {
		return runtimeWaitableInfo{}, false
	}
	return runtimeWaitableInfo{
		Direction: node.Direction,
		Elem:      underlying.derived(node.Elem),
	}, true
}

func (t vmType) FunctionInfo() (runtimeFunctionInfo, bool) {
	if t.Ref.Kind == types.Primitive && t.Ref.Primitive == types.PrimitiveFunction {
		return runtimeFunctionInfo{}, true
	}
	underlying := t.Underlying()
	if underlying.Ref.Kind != types.Function {
		return runtimeFunctionInfo{}, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != types.Function || node.Signature == nil {
		return runtimeFunctionInfo{}, false
	}
	return runtimeFunctionInfo{Signature: *node.Signature}, true
}

func (t vmType) InterfaceInfo() (runtimeInterfaceInfo, bool) {
	if t.Ref.Kind == types.Named && t.Underlying().Ref.Kind == types.Any {
		return runtimeInterfaceInfo{}, true
	}
	underlying := t.Underlying()
	if underlying.Ref.Kind != types.Interface {
		return runtimeInterfaceInfo{}, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != types.Interface {
		return runtimeInterfaceInfo{}, false
	}
	return runtimeInterfaceInfo{
		Methods: node.Methods,
		Terms:   node.Terms,
		TypeSet: node.TypeSet,
	}, true
}

func (t vmType) ShapeKind() types.Kind {
	if t.Ref.Kind != types.Named {
		return t.Ref.Kind
	}
	underlying := t.Underlying().Ref.Kind
	if underlying == types.Any {
		return types.Interface
	}
	return underlying
}

func (t vmType) PointerElem() (vmType, bool) {
	return runtimeTypeElem(t, types.Pointer)
}

func (t vmType) SliceElem() (vmType, bool) {
	return runtimeTypeElem(t, types.Slice)
}

func (t vmType) ArrayInfo() (uint64, vmType, bool) {
	underlying := t.Underlying()
	if underlying.Ref.Kind != types.Array {
		return 0, vmType{}, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != types.Array || !node.Elem.Valid() {
		return 0, vmType{}, false
	}
	if node.Length < 0 {
		return 0, vmType{}, false
	}
	return uint64(node.Length), underlying.derived(node.Elem), true
}

func (t vmType) MapInfo() (vmType, vmType, bool) {
	underlying := t.Underlying()
	if underlying.Ref.Kind != types.Map {
		return vmType{}, vmType{}, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != types.Map || !node.Key.Valid() || !node.Elem.Valid() {
		return vmType{}, vmType{}, false
	}
	return underlying.derived(node.Key), underlying.derived(node.Elem), true
}

func (t vmType) StructFields() ([]types.Field, bool) {
	underlying := t.Underlying()
	if underlying.Ref.Kind != types.Struct {
		return nil, false
	}
	node, ok := underlying.Node()
	if !ok || node.Kind != types.Struct {
		return nil, false
	}
	return node.Fields, true
}

func runtimeStructFieldTypes(runtimeType vmType) ([]structFieldType, bool) {
	fields, ok := runtimeType.StructFields()
	if !ok {
		return nil, false
	}
	out := make([]structFieldType, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		fieldType := strings.TrimSpace(types.FormatWithTable(runtimeType.Table, field.Type))
		if name == "" || name == "_" || fieldType == "" {
			continue
		}
		out = append(out, structFieldType{Name: name, Type: fieldType})
	}
	return out, true
}
