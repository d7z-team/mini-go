package runtime

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

type TypeInfo struct {
	Key           string
	ModulePath    string
	Package       string
	PkgPath       string
	ID            string
	Name          string
	QualifiedName string
	Display       string
	Kind          string
	Type          string
	QualifiedType string
	Variadic      bool
	Exported      bool
	Align         int
	FieldAlign    int
	Size          uint64
	Bits          int
	ChanDir       int
	Fields        []TypeFieldInfo
	Methods       []TypeMethodInfo
}

type TypeFieldInfo struct {
	Name        string
	PkgPath     string
	RuntimeType vmType
	Variadic    bool
	Tag         string
	Embedded    bool
	Offset      uint64
	Exported    bool
}

type TypeMethodInfo struct {
	ModulePath    string
	Name          string
	PkgPath       string
	ReceiverType  vmType
	SignatureType vmType
	Variadic      bool
	FunctionID    string
	Exported      bool
}

func (r *moduleRegistry) findType(modulePath, name string) (TypeInfo, bool) {
	if r == nil {
		return TypeInfo{}, false
	}
	module, ok := r.modules[strings.TrimSpace(modulePath)]
	if !ok {
		return TypeInfo{}, false
	}
	return module.findType(name)
}

func (vm *vm) findType(modulePath, name string) (TypeInfo, bool) {
	if vm == nil {
		return TypeInfo{}, false
	}
	modulePath = strings.TrimSpace(modulePath)
	root := vm.rootModule()
	if root != nil && modulePath == root.executable.Artifact.Module.Path {
		return root.findType(name)
	}
	return vm.moduleRegistry().findType(modulePath, name)
}

func (m *moduleInstance) findType(name string) (TypeInfo, bool) {
	if m == nil || m.executable == nil {
		return TypeInfo{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return TypeInfo{}, false
	}
	decl, ok := m.executable.Types[name]
	if !ok {
		return TypeInfo{}, false
	}
	if decl.Alias && decl.AliasTarget.Valid() {
		resolved := m.resolvedRuntimeType(m.runtimeType(decl.AliasTarget))
		return reflectTypeInfoForTypeText(m, resolved.String()), true
	}
	return m.typeInfo(decl), true
}

func (m *moduleInstance) typeInfo(decl types.TypeNode) TypeInfo {
	if m != nil {
		if cached, ok := m.typeInfoCache.load(decl.ID); ok {
			return cached
		}
	}
	modulePath := ""
	packageName := ""
	if m != nil && m.executable != nil {
		modulePath = m.executable.Artifact.Module.Path
		packageName = m.executable.Artifact.Module.Package
	}
	name := strings.TrimSpace(string(decl.Identity.DeclID))
	ref := types.Ref(decl)
	variadic := false
	if signature, ok := m.executable.Artifact.TypeTable.IsFunction(ref); ok {
		variadic = signature.Variadic
	}
	info := TypeInfo{
		Key:           qualifiedTypeName(modulePath, name),
		ModulePath:    modulePath,
		Package:       packageName,
		PkgPath:       packagePathForType(modulePath, name),
		ID:            string(decl.ID),
		Name:          name,
		QualifiedName: qualifiedTypeName(modulePath, name),
		Display:       reflectSourceType(m, m.runtimeType(ref)),
		Kind:          canonicalTypeKind(types.FormatWithTable(&m.executable.Artifact.TypeTable, decl.Underlying)),
		Type:          types.FormatWithTable(&m.executable.Artifact.TypeTable, decl.Underlying),
		QualifiedType: m.qualifyLocalType(types.FormatWithTable(&m.executable.Artifact.TypeTable, decl.Underlying)),
		Variadic:      variadic,
		Exported:      isExportedName(name),
		Fields:        m.typeFieldInfo(m.executable.typeFields(decl)),
		Methods:       m.typeMethodInfo(decl.Methods),
	}
	declType := types.FormatWithTable(&m.executable.Artifact.TypeTable, decl.Underlying)
	info.Align, info.FieldAlign, info.Size, info.Bits, info.ChanDir = reflectTypeLayoutValues(m.reflectTypeLayout(declType))
	if len(info.Fields) == 0 && info.Kind == "struct" {
		info.Fields = m.canonicalStructFieldInfo(declType)
	}
	if info.Kind != "interface" {
		exported := info.Methods[:0]
		for _, method := range info.Methods {
			if method.Exported {
				exported = append(exported, method)
			}
		}
		info.Methods = exported
	}
	if m != nil {
		m.typeInfoCache.store(decl.ID, info)
	}
	return info
}

func packagePathForType(modulePath, name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return strings.TrimSpace(modulePath)
}

func (m *moduleInstance) typeFieldInfo(fields []types.Field) []TypeFieldInfo {
	if len(fields) == 0 {
		return nil
	}
	out := make([]TypeFieldInfo, 0, len(fields))
	offset := uint64(0)
	for _, field := range fields {
		layout := m.reflectRuntimeTypeLayoutSeen(vmType{Ref: field.Type, Table: &m.executable.Artifact.TypeTable}, map[string]struct{}{})
		if layout.align < 1 {
			layout.align = 1
		}
		offset = reflectAlignUp(offset, uint64(layout.align))
		variadic := false
		if signature, ok := m.executable.Artifact.TypeTable.IsFunction(field.Type); ok {
			variadic = signature.Variadic
		}
		out = append(out, TypeFieldInfo{
			Name:        strings.TrimSpace(field.Name),
			PkgPath:     packagePathForField(m.modulePath(), field.Name),
			RuntimeType: m.resolvedRuntimeType(vmType{Ref: field.Type, Table: &m.executable.Artifact.TypeTable}),
			Variadic:    variadic,
			Tag:         field.Tag,
			Embedded:    field.Embedded,
			Offset:      offset,
			Exported:    isExportedName(field.Name),
		})
		offset += layout.size
	}
	return out
}

func (m *moduleInstance) moduleStructFieldInfo(typ any) ([]TypeFieldInfo, bool) {
	typeText := strings.TrimSpace(m.resolvedRuntimeType(typ).String())
	if elem, ok := m.resolvedRuntimeType(typ).PointerElem(); ok {
		typeText = elem.String()
	}
	if typeText == "" {
		return nil, false
	}
	if cached, ok := m.structFieldsCache.load(typeText); ok {
		return cached.fields, cached.found
	}

	resolve := func(fields []TypeFieldInfo, found bool) ([]TypeFieldInfo, bool) {
		m.structFieldsCache.store(typeText, structFieldsResolution{fields: fields, found: found})
		return fields, found
	}
	if canonicalTypeKind(typeText) == "struct" {
		return resolve(m.canonicalStructFieldInfo(typeText), true)
	}
	if m == nil || m.executable == nil {
		return resolve(nil, false)
	}
	if target, name, ok := m.qualifiedTypeModule(typeText); ok {
		if target != m {
			return target.moduleStructFieldInfo(name)
		}
		typeText = name
	}
	decl, ok := m.executable.Types[typeText]
	if !ok {
		return resolve(nil, false)
	}
	if fields := m.executable.typeFields(decl); len(fields) != 0 {
		return resolve(m.typeFieldInfo(fields), true)
	}
	declType := types.FormatWithTable(&m.executable.Artifact.TypeTable, decl.Underlying)
	if canonicalTypeKind(declType) == "struct" {
		return resolve(m.canonicalStructFieldInfo(declType), true)
	}
	return resolve(nil, false)
}

func (m *moduleInstance) canonicalStructFieldInfo(typ string) []TypeFieldInfo {
	fields := parseStructFields(typ)
	out := make([]TypeFieldInfo, 0, len(fields))
	offset := uint64(0)
	for _, field := range fields {
		fieldType := strings.TrimSpace(field.Type)
		layout := m.reflectTypeLayout(fieldType)
		if layout.align < 1 {
			layout.align = 1
		}
		offset = reflectAlignUp(offset, uint64(layout.align))
		out = append(out, TypeFieldInfo{
			Name:        strings.TrimSpace(field.Name),
			PkgPath:     packagePathForField(m.modulePath(), field.Name),
			RuntimeType: m.resolvedRuntimeType(fieldType),
			Variadic:    false,
			Tag:         field.Tag,
			Embedded:    field.Embedded,
			Offset:      offset,
			Exported:    isExportedName(field.Name),
		})
		offset += layout.size
	}
	return out
}

func (m *moduleInstance) typeMethodInfo(methods []types.Method) []TypeMethodInfo {
	if len(methods) == 0 {
		return nil
	}
	out := make([]TypeMethodInfo, 0, len(methods))
	for _, method := range methods {
		modulePath := strings.TrimSpace(method.ModulePath)
		if modulePath == "" {
			modulePath = m.modulePath()
		}
		receiver := types.FormatWithTable(&m.executable.Artifact.TypeTable, method.Receiver)
		signature := types.FormatSignature(&m.executable.Artifact.TypeTable, method.Signature)
		if normalized, _, ok := m.functionTypeInfo(signature); ok {
			signature = m.localizeType(normalized)
		}
		out = append(out, TypeMethodInfo{
			ModulePath: modulePath, Name: strings.TrimSpace(method.Name),
			PkgPath:       packagePathForField(modulePath, method.Name),
			ReceiverType:  m.resolvedRuntimeType(m.qualifyLocalType(receiver)),
			SignatureType: m.resolvedRuntimeType(m.qualifyLocalType(signature)),
			Variadic:      method.Signature.Variadic, FunctionID: strings.TrimSpace(method.FunctionID),
			Exported: isExportedName(method.Name),
		})
	}
	sortTypeMethodInfo(out)
	return out
}

func packagePathForField(modulePath, name string) string {
	if isExportedName(name) {
		return ""
	}
	return strings.TrimSpace(modulePath)
}

func qualifiedTypeName(modulePath, name string) string {
	modulePath = strings.TrimSpace(modulePath)
	name = strings.TrimSpace(name)
	if modulePath == "" || name == "" {
		return name
	}
	return modulePath + "." + name
}

func canonicalTypeKind(typ string) string {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return ""
	}
	runtimeType := coerceRuntimeType(typ)
	if !runtimeType.Valid() {
		return "named"
	}
	node, hasNode := runtimeType.Node()
	switch {
	case runtimeType.Ref.Kind == types.Struct || (hasNode && node.Kind == types.Struct):
		return "struct"
	case runtimeType.Ref.Kind == types.Interface || (hasNode && node.Kind == types.Interface):
		return "interface"
	case runtimeType.Ref.Kind == types.Slice || (hasNode && node.Kind == types.Slice):
		return "slice"
	case runtimeType.Ref.Kind == types.Array || (hasNode && node.Kind == types.Array):
		return "array"
	case runtimeType.Ref.Kind == types.Map || (hasNode && node.Kind == types.Map):
		return "map"
	case runtimeType.Ref.Kind == types.Pointer || (hasNode && node.Kind == types.Pointer):
		return "pointer"
	case runtimeType.Ref.Kind == types.Function || (hasNode && node.Kind == types.Function):
		return "function"
	case hasNode && node.Kind == types.Waitable && node.Direction == types.ChannelReceive:
		return "recv_chan"
	case hasNode && node.Kind == types.Waitable && node.Direction == types.ChannelSend:
		return "send_chan"
	case runtimeType.Ref.Kind == types.Waitable || (hasNode && node.Kind == types.Waitable && node.Direction == types.ChannelBoth):
		return "chan"
	case runtimeType.Ref.Kind == types.Void || runtimeType.Ref.Kind == types.Any || runtimeType.Ref.Kind == types.Primitive:
		return "primitive"
	default:
		return "named"
	}
}

func isPrimitiveTypeName(name string) bool {
	switch strings.TrimSpace(name) {
	case "Void", "Any":
		return true
	default:
		return runtimeTypeFromText(strings.TrimSpace(name)).Ref.Kind == types.Primitive
	}
}

func isExportedName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	ch := name[0]
	return ch >= 'A' && ch <= 'Z'
}

func sortTypeMethodInfo(values []TypeMethodInfo) {
	for i := 1; i < len(values); i++ {
		value := values[i]
		j := i - 1
		for j >= 0 && typeMethodInfoLess(value, values[j]) {
			values[j+1] = values[j]
			j--
		}
		values[j+1] = value
	}
}

func typeMethodInfoLess(left, right TypeMethodInfo) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.ReceiverType.String() != right.ReceiverType.String() {
		return left.ReceiverType.String() < right.ReceiverType.String()
	}
	return left.SignatureType.String() < right.SignatureType.String()
}
