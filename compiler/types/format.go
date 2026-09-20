package types

import (
	"fmt"
	"strings"
)

func KindName(kind Kind) string {
	switch kind {
	case Void:
		return "void"
	case Any:
		return "any"
	case Primitive:
		return "primitive"
	case Named:
		return "named"
	case Slice:
		return "slice"
	case Array:
		return "array"
	case Map:
		return "map"
	case Pointer:
		return "pointer"
	case Waitable:
		return "waitable"
	case Function:
		return "function"
	case Tuple:
		return "tuple"
	case Struct:
		return "struct"
	case Interface:
		return "interface"
	case TypeParameter:
		return "type_parameter"
	case Instance:
		return "instance"
	default:
		return "invalid"
	}
}

func PrimitiveName(kind PrimitiveKind) string {
	switch kind {
	case PrimitiveBool:
		return "Bool"
	case PrimitiveString:
		return "String"
	case PrimitiveInt:
		return "Int"
	case PrimitiveInt8:
		return "Int8"
	case PrimitiveInt16:
		return "Int16"
	case PrimitiveInt32:
		return "Int32"
	case PrimitiveInt64:
		return "Int64"
	case PrimitiveUint:
		return "Uint"
	case PrimitiveUint8:
		return "Uint8"
	case PrimitiveUint16:
		return "Uint16"
	case PrimitiveUint32:
		return "Uint32"
	case PrimitiveUint64:
		return "Uint64"
	case PrimitiveUintptr:
		return "Uintptr"
	case PrimitiveFloat32:
		return "Float32"
	case PrimitiveFloat64:
		return "Float64"
	case PrimitiveComplex64:
		return "Complex64"
	case PrimitiveComplex128:
		return "Complex128"
	case PrimitiveError:
		return "Error"
	case PrimitiveFunction:
		return "Function"
	default:
		return ""
	}
}

func Format(ref TypeRef) string {
	return formatRef(ref, nil, map[TypeID]bool{})
}

func FormatWithTable(table *TypeTable, ref TypeRef) string {
	if table == nil {
		return formatRef(ref, nil, map[TypeID]bool{})
	}
	if table.indexOwner != table {
		if err := table.index(); err != nil {
			return formatRef(ref, table, map[TypeID]bool{})
		}
	}
	table.cache.RLock()
	formatted, ok := table.cache.formatted[ref]
	table.cache.RUnlock()
	if ok {
		return formatted
	}
	formatted = formatRef(ref, table, map[TypeID]bool{})
	table.cache.Lock()
	table.cache.formatted[ref] = formatted
	table.cache.Unlock()
	return formatted
}

func FormatSignature(table *TypeTable, signature FunctionSignature) string {
	return formatFunctionSignature(table, signature, map[TypeID]bool{})
}

func formatRef(ref TypeRef, table *TypeTable, seen map[TypeID]bool) string {
	if ref.Kind == Void {
		return "Void"
	}
	if ref.Kind == Any {
		return "Any"
	}
	if ref.Kind == Primitive {
		return PrimitiveName(ref.Primitive)
	}
	if ref.Kind == Named {
		return ref.Named.ModulePath + "." + string(ref.Named.DeclID)
	}
	if table == nil || ref.Node == "" || seen[ref.Node] {
		return KindName(ref.Kind)
	}
	node, ok := table.Node(ref)
	if !ok {
		return KindName(ref.Kind)
	}
	seen[ref.Node] = true
	defer delete(seen, ref.Node)
	switch node.Kind {
	case Slice:
		return "Slice<" + formatRef(node.Elem, table, seen) + ">"
	case Array:
		return fmt.Sprintf("Array<%d, %s>", node.Length, formatRef(node.Elem, table, seen))
	case Map:
		return "Map<" + formatRef(node.Key, table, seen) + ", " + formatRef(node.Elem, table, seen) + ">"
	case Pointer:
		return "Ptr<" + formatRef(node.Elem, table, seen) + ">"
	case Waitable:
		prefix := "Waitable<"
		switch node.Direction {
		case ChannelReceive:
			prefix = "ReceiveWaitable<"
		case ChannelSend:
			prefix = "SendWaitable<"
		}
		return prefix + formatRef(node.Elem, table, seen) + ">"
	case Function:
		if node.Signature == nil {
			return "function() Void"
		}
		return formatFunctionSignature(table, *node.Signature, seen)
	case Struct:
		fields := make([]string, len(node.Fields))
		for i, field := range node.Fields {
			prefix := ""
			if field.Embedded {
				prefix = "embedded "
			}
			fields[i] = prefix + field.Name + ":" + formatRef(field.Type, table, seen)
			if field.Tag != "" {
				fields[i] += " `" + field.Tag + "`"
			}
		}
		return "struct{" + strings.Join(fields, ",") + "}"
	case Interface:
		if node.Name == "comparable" && node.TypeSet && len(node.Terms) == 0 && len(node.Methods) == 0 {
			return "Comparable"
		}
		members := make([]string, 0, len(node.Terms)+len(node.Methods))
		union := make([]string, 0)
		for _, term := range node.Terms {
			member := formatRef(term.Type, table, seen)
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
			members = append(members, method.Name+":"+formatFunctionSignature(table, method.Signature, seen))
		}
		return "interface{" + strings.Join(members, ",") + "}"
	case TypeParameter:
		if node.Name != "" {
			return node.Name
		}
		return KindName(node.Kind)
	case Instance:
		return formatRef(node.Base, table, seen)
	default:
		if !emptyTypeKey(node.Identity) {
			return node.Identity.ModulePath + "." + string(node.Identity.DeclID)
		}
		return KindName(node.Kind)
	}
}

func formatFunctionSignature(table *TypeTable, signature FunctionSignature, seen map[TypeID]bool) string {
	params := make([]string, len(signature.Params))
	for i, param := range signature.Params {
		params[i] = formatRef(param.Type, table, seen)
		if signature.Variadic && i == len(params)-1 {
			params[i] = "variadic " + params[i]
		}
	}
	results := make([]string, len(signature.Results))
	for i, result := range signature.Results {
		results[i] = formatRef(result, table, seen)
	}
	result := ""
	if len(results) == 1 {
		result = " " + results[0]
	} else if len(results) > 1 {
		result = " tuple(" + strings.Join(results, ", ") + ")"
	}
	return "function(" + strings.Join(params, ", ") + ")" + result
}
