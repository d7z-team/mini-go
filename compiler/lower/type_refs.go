package lower

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) initTypeRefs() {
	if l.typeTable == nil {
		l.typeTable = &types.TypeTable{}
	}
	if l.typeParser == nil {
		l.typeParser = types.NewParser(l.modulePath, l.typeTable)
	}
	if l.typeRefs == nil {
		l.typeRefs = map[string]types.TypeRef{}
	}
	l.relations = types.NewRelations(l.typeTable)
}

func (l *lowerer) typeRef(text string) (types.TypeRef, bool) {
	l.initTypeRefs()
	if ref, ok := l.typeRefs[text]; ok {
		return ref, ref.Valid()
	}
	canonical := strings.TrimSpace(l.resolveType(text))
	if canonical == "" {
		return types.TypeRef{}, false
	}
	if ref, ok := l.typeRefs[canonical]; ok {
		return ref, ref.Valid()
	}
	ref, err := l.typeParser.Parse(canonical)
	if err != nil || !ref.Valid() {
		return types.TypeRef{}, false
	}
	l.typeRefs[canonical] = ref
	return ref, true
}

func (l *lowerer) typeView(text string) (types.TypeView, bool) {
	ref, ok := l.typeRef(text)
	if !ok {
		return types.TypeView{}, false
	}
	return types.View(l.typeTable, ref), true
}

func (l *lowerer) typeRelations() types.Relations {
	l.initTypeRefs()
	return l.relations
}

func (l *lowerer) typeRefString(ref types.TypeRef) string {
	if !ref.Valid() {
		return ""
	}
	if text, ok := l.typeTextCache[ref]; ok {
		return text
	}
	text := l.formatTypeRef(ref, map[types.TypeID]bool{})
	if text != "" && l.typeTextCache != nil {
		l.typeTextCache[ref] = text
	}
	return text
}

func (l *lowerer) typeStringsFromRefs(refs []types.TypeRef) []string {
	texts := make([]string, len(refs))
	for i, ref := range refs {
		texts[i] = l.typeRefString(ref)
	}
	return texts
}

func (l *lowerer) signatureParamTypes(signature types.FunctionSignature) []string {
	texts := make([]string, len(signature.Params))
	for i, param := range signature.Params {
		texts[i] = l.typeRefString(param.Type)
	}
	return texts
}

func (l *lowerer) signatureResultTypes(signature types.FunctionSignature) []string {
	return l.typeStringsFromRefs(signature.Results)
}

func (l *lowerer) signatureString(signature types.FunctionSignature) string {
	return l.formatFunctionSignature(signature, map[types.TypeID]bool{})
}

func (l *lowerer) formatTypeRef(ref types.TypeRef, seen map[types.TypeID]bool) string {
	switch ref.Kind {
	case types.Void:
		return "Void"
	case types.Any:
		return "Any"
	case types.Primitive:
		return types.PrimitiveName(ref.Primitive)
	case types.Named:
		return l.formatNamedType(ref.Named)
	}
	if l.typeTable == nil || ref.Node == "" || seen[ref.Node] {
		return types.KindName(ref.Kind)
	}
	node, ok := l.typeTable.Node(ref)
	if !ok {
		return types.KindName(ref.Kind)
	}
	seen[ref.Node] = true
	defer delete(seen, ref.Node)
	switch node.Kind {
	case types.Slice:
		return "Slice<" + l.formatTypeRef(node.Elem, seen) + ">"
	case types.Array:
		if node.Length == types.UnknownArrayLength {
			return ""
		}
		return "Array<" + strconv.FormatInt(node.Length, 10) + ", " + l.formatTypeRef(node.Elem, seen) + ">"
	case types.Map:
		return "Map<" + l.formatTypeRef(node.Key, seen) + ", " + l.formatTypeRef(node.Elem, seen) + ">"
	case types.Pointer:
		return "Ptr<" + l.formatTypeRef(node.Elem, seen) + ">"
	case types.Waitable:
		prefix := "Waitable<"
		switch node.Direction {
		case types.ChannelReceive:
			prefix = "ReceiveWaitable<"
		case types.ChannelSend:
			prefix = "SendWaitable<"
		}
		return prefix + l.formatTypeRef(node.Elem, seen) + ">"
	case types.Function:
		if node.Signature == nil {
			return "function() Void"
		}
		return l.formatFunctionSignature(*node.Signature, seen)
	case types.Tuple:
		parts := make([]string, len(node.Tuple))
		for i, elem := range node.Tuple {
			parts[i] = l.formatTypeRef(elem, seen)
		}
		return "tuple(" + strings.Join(parts, ", ") + ")"
	case types.Struct:
		fields := make([]string, len(node.Fields))
		for i, field := range node.Fields {
			fields[i] = field.Name + ":" + l.formatTypeRef(field.Type, seen)
			if field.Tag != "" {
				fields[i] += " `" + field.Tag + "`"
			}
		}
		return "struct{" + strings.Join(fields, ",") + "}"
	case types.Interface:
		members := make([]string, 0, len(node.Terms)+len(node.Methods))
		union := make([]string, 0)
		for _, term := range node.Terms {
			member := l.formatTypeRef(term.Type, seen)
			if term.Approx {
				member = "~" + member
			}
			if term.Union {
				union = append(union, member)
			} else {
				members = append(members, member)
			}
		}
		if len(union) != 0 {
			members = append([]string{strings.Join(union, "|")}, members...)
		}
		for _, method := range node.Methods {
			members = append(members, method.Name+":"+l.formatFunctionSignature(method.Signature, seen))
		}
		return "interface{" + strings.Join(members, ",") + "}"
	default:
		if node.Identity.DeclID != "" {
			return l.formatNamedType(node.Identity)
		}
		return types.KindName(node.Kind)
	}
}

func (l *lowerer) formatFunctionSignature(signature types.FunctionSignature, seen map[types.TypeID]bool) string {
	params := make([]string, len(signature.Params))
	for i, param := range signature.Params {
		params[i] = l.formatTypeRef(param.Type, seen)
		if signature.Variadic && i == len(params)-1 {
			params[i] = "variadic " + params[i]
		}
	}
	results := make([]string, len(signature.Results))
	for i, result := range signature.Results {
		results[i] = l.formatTypeRef(result, seen)
	}
	result := ""
	if len(results) == 1 {
		result = " " + results[0]
	} else if len(results) > 1 {
		result = " tuple(" + strings.Join(results, ", ") + ")"
	}
	return "function(" + strings.Join(params, ", ") + ")" + result
}

func (l *lowerer) formatNamedType(named types.TypeKey) string {
	name := string(named.DeclID)
	if name == "" {
		return ""
	}
	if l.isCurrentModulePath(named.ModulePath) {
		if types.IsBuiltinTypeName(name) {
			return named.ModulePath + "." + name
		}
		return name
	}
	if named.ModulePath == "" {
		return name
	}
	return named.ModulePath + "." + name
}

func (l *lowerer) isCurrentModulePath(modulePath string) bool {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" {
		return false
	}
	if modulePath == strings.TrimSpace(l.modulePath) {
		return true
	}
	return l.program != nil && modulePath == strings.TrimSpace(l.program.ModulePath)
}
