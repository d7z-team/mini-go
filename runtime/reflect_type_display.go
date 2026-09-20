package runtime

import (
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

// reflectSourceType renders the source-facing type spelling used by
// reflect.Type.String. Runtime identity continues to use the structured
// TypeRef and its canonical key.
func reflectSourceType(module *moduleInstance, typ vmType) string {
	return reflectSourceTypeRef(module, typ.Table, typ.Ref, make(map[types.TypeID]bool))
}

func reflectSourceTypeRef(module *moduleInstance, table *types.TypeTable, ref types.TypeRef, seen map[types.TypeID]bool) string {
	switch ref.Kind {
	case types.Void:
		return ""
	case types.Any:
		return "any"
	case types.Primitive:
		return reflectPrimitiveSourceName(ref.Primitive)
	case types.Named:
		return reflectNamedSourceName(module, ref.Named)
	}
	if table == nil || ref.Node == "" || seen[ref.Node] {
		return strings.ToLower(types.KindName(ref.Kind))
	}
	node, ok := table.Node(ref)
	if !ok {
		return strings.ToLower(types.KindName(ref.Kind))
	}
	seen[ref.Node] = true
	defer delete(seen, ref.Node)
	render := func(child types.TypeRef) string {
		return reflectSourceTypeRef(module, table, child, seen)
	}
	switch node.Kind {
	case types.Slice:
		return "[]" + render(node.Elem)
	case types.Array:
		return "[" + strconv.FormatInt(node.Length, 10) + "]" + render(node.Elem)
	case types.Map:
		return "map[" + render(node.Key) + "]" + render(node.Elem)
	case types.Pointer:
		return "*" + render(node.Elem)
	case types.Waitable:
		prefix := "chan "
		switch node.Direction {
		case types.ChannelReceive:
			prefix = "<-chan "
		case types.ChannelSend:
			prefix = "chan<- "
		}
		return prefix + render(node.Elem)
	case types.Function:
		if node.Signature == nil {
			return "func()"
		}
		return reflectSourceFunction(module, table, *node.Signature, seen)
	case types.Struct:
		fields := make([]string, 0, len(node.Fields))
		for _, field := range node.Fields {
			fieldType := render(field.Type)
			text := fieldType
			if !field.Embedded {
				text = field.Name + " " + fieldType
			}
			if field.Tag != "" {
				text += " " + strconv.Quote(field.Tag)
			}
			fields = append(fields, text)
		}
		return "struct { " + strings.Join(fields, "; ") + " }"
	case types.Interface:
		members := make([]string, 0, len(node.Terms)+len(node.Methods))
		for _, term := range node.Terms {
			text := render(term.Type)
			if term.Approx {
				text = "~" + text
			}
			members = append(members, text)
		}
		for _, method := range node.Methods {
			signature := reflectSourceFunction(module, table, method.Signature, seen)
			members = append(members, method.Name+strings.TrimPrefix(signature, "func"))
		}
		return "interface { " + strings.Join(members, "; ") + " }"
	case types.Instance:
		arguments := make([]string, len(node.TypeArgs))
		for i, argument := range node.TypeArgs {
			arguments[i] = render(argument)
		}
		return render(node.Base) + "[" + strings.Join(arguments, ", ") + "]"
	default:
		return strings.ToLower(types.KindName(node.Kind))
	}
}

func reflectSourceFunction(module *moduleInstance, table *types.TypeTable, signature types.FunctionSignature, seen map[types.TypeID]bool) string {
	params := make([]string, len(signature.Params))
	for i, param := range signature.Params {
		params[i] = reflectSourceTypeRef(module, table, param.Type, seen)
		if signature.Variadic && i == len(params)-1 {
			params[i] = "..." + strings.TrimPrefix(params[i], "[]")
		}
	}
	results := make([]string, len(signature.Results))
	for i, result := range signature.Results {
		results[i] = reflectSourceTypeRef(module, table, result, seen)
	}
	out := "func(" + strings.Join(params, ", ") + ")"
	if len(results) == 1 {
		return out + " " + results[0]
	}
	if len(results) > 1 {
		return out + " (" + strings.Join(results, ", ") + ")"
	}
	return out
}

func reflectNamedSourceName(module *moduleInstance, key types.TypeKey) string {
	name := string(key.DeclID)
	path := strings.TrimSpace(key.ModulePath)
	if path == "" {
		return name
	}
	packageName := ""
	if module != nil {
		if module.modulePath() == path && module.executable != nil {
			packageName = module.executable.Artifact.Module.Package
		} else if module.registry != nil {
			if owner, ok := module.registry.module(path); ok && owner.executable != nil {
				packageName = owner.executable.Artifact.Module.Package
			}
		}
	}
	if packageName == "" {
		packageName = path
		if slash := strings.LastIndex(packageName, "/"); slash >= 0 {
			packageName = packageName[slash+1:]
		}
	}
	return packageName + "." + name
}

func reflectPrimitiveSourceName(kind types.PrimitiveKind) string {
	name := types.PrimitiveName(kind)
	switch kind {
	case types.PrimitiveError:
		return "error"
	case types.PrimitiveFunction:
		return "func"
	default:
		return strings.ToLower(name)
	}
}
