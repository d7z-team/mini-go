package types

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Parser is the compiler input boundary for the current source lowerer. Runtime
// packages never use it; they consume the validated TypeTable from an artifact.
type Parser struct {
	ModulePath string
	Table      *TypeTable
	// Bindings resolves declaration-scoped type parameters in source signatures.
	Bindings map[string]TypeRef
}

func NewParser(modulePath string, table *TypeTable) *Parser {
	if table == nil {
		table = &TypeTable{}
	}
	return &Parser{ModulePath: strings.TrimSpace(modulePath), Table: table}
}

// IsBuiltinTypeName reports whether name has a canonical non-named meaning.
// A source declaration that shadows one of these names must stay qualified at
// canonical text boundaries so its named identity is not lost.
func IsBuiltinTypeName(name string) bool {
	name = strings.TrimSpace(name)
	return name == "Void" || name == "Any" || primitiveByName(name) != PrimitiveInvalid
}

// PrimitiveByName resolves a canonical primitive name without allocating a
// parser or mutating a type table.
func PrimitiveByName(name string) (PrimitiveKind, bool) {
	primitive := primitiveByName(strings.TrimSpace(name))
	return primitive, primitive != PrimitiveInvalid
}

func (p *Parser) Define(name string, underlying TypeRef, alias bool) (TypeRef, error) {
	name = strings.TrimSpace(name)
	if name == "" || !underlying.Valid() {
		return TypeRef{}, fmt.Errorf("invalid type definition %q", name)
	}
	key := TypeKey{ModulePath: p.ModulePath, DeclID: DeclID(name)}
	id := TypeID("decl." + p.ModulePath + "." + name)
	node := TypeNode{ID: id, Kind: Named, Identity: key, Alias: alias, Underlying: underlying}
	if alias {
		node.AliasTarget = underlying
		node.Underlying = TypeRef{}
	}
	if existing, ok := p.Table.Named(key); ok {
		if existing.ID != id {
			return TypeRef{}, fmt.Errorf("duplicate type definition %s", name)
		}
		return TypeRef{Kind: Named, Named: key, Node: id}, nil
	}
	if err := p.Table.Add(node); err != nil {
		return TypeRef{}, err
	}
	return TypeRef{Kind: Named, Named: key, Node: id}, nil
}

func (p *Parser) Parse(text string) (TypeRef, error) {
	text = strings.TrimSpace(text)
	if ref, ok := p.Bindings[text]; ok {
		return ref, nil
	}
	if text == "" {
		return TypeRef{}, errors.New("empty type")
	}
	switch text {
	case "Void":
		return VoidType(), nil
	case "Any":
		return AnyType(), nil
	}
	if primitive := primitiveByName(text); primitive != PrimitiveInvalid {
		return Builtin(primitive), nil
	}
	if strings.HasPrefix(text, "Slice<") && strings.HasSuffix(text, ">") {
		elem, err := p.Parse(text[len("Slice<") : len(text)-1])
		if err != nil {
			return TypeRef{}, err
		}
		return p.node(TypeNode{Kind: Slice, Elem: elem}, text)
	}
	if strings.HasPrefix(text, "Ptr<") && strings.HasSuffix(text, ">") {
		elem, err := p.Parse(text[len("Ptr<") : len(text)-1])
		if err != nil {
			return TypeRef{}, err
		}
		return p.node(TypeNode{Kind: Pointer, Elem: elem}, text)
	}
	for prefix, direction := range map[string]ChannelDir{"Waitable<": ChannelBoth, "ReceiveWaitable<": ChannelReceive, "SendWaitable<": ChannelSend} {
		if strings.HasPrefix(text, prefix) && strings.HasSuffix(text, ">") {
			elem, err := p.Parse(text[len(prefix) : len(text)-1])
			if err != nil {
				return TypeRef{}, err
			}
			return p.node(TypeNode{Kind: Waitable, Direction: direction, Elem: elem}, text)
		}
	}
	if strings.HasPrefix(text, "Array<") && strings.HasSuffix(text, ">") {
		parts := splitTopLevel(text[len("Array<"):len(text)-1], ',')
		if len(parts) != 2 {
			return TypeRef{}, fmt.Errorf("malformed array type %q", text)
		}
		length, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil || length < 0 {
			return TypeRef{}, fmt.Errorf("invalid array length in %q", text)
		}
		elem, err := p.Parse(parts[1])
		if err != nil {
			return TypeRef{}, err
		}
		return p.node(TypeNode{Kind: Array, Length: length, Elem: elem}, text)
	}
	if strings.HasPrefix(text, "Map<") && strings.HasSuffix(text, ">") {
		parts := splitTopLevel(text[len("Map<"):len(text)-1], ',')
		if len(parts) != 2 {
			return TypeRef{}, fmt.Errorf("malformed map type %q", text)
		}
		key, err := p.Parse(parts[0])
		if err != nil {
			return TypeRef{}, err
		}
		elem, err := p.Parse(parts[1])
		if err != nil {
			return TypeRef{}, err
		}
		return p.node(TypeNode{Kind: Map, Key: key, Elem: elem}, text)
	}
	if strings.HasPrefix(text, "tuple(") && strings.HasSuffix(text, ")") {
		var refs []TypeRef
		inner := strings.TrimSpace(text[len("tuple(") : len(text)-1])
		if inner != "" {
			for _, part := range splitTopLevel(inner, ',') {
				ref, err := p.Parse(part)
				if err != nil {
					return TypeRef{}, err
				}
				refs = append(refs, ref)
			}
		}
		return p.node(TypeNode{Kind: Tuple, Tuple: refs}, text)
	}
	if strings.HasPrefix(text, "function(") {
		signature, err := p.parseFunction(text)
		if err != nil {
			return TypeRef{}, err
		}
		return p.node(TypeNode{Kind: Function, Signature: &signature}, text)
	}
	if strings.HasPrefix(text, "struct{") && strings.HasSuffix(text, "}") {
		return p.parseStruct(text)
	}
	if strings.HasPrefix(text, "interface{") && strings.HasSuffix(text, "}") {
		return p.parseInterface(text)
	}
	if strings.Contains(text, ".") {
		separator := strings.LastIndexByte(text, '.')
		modulePath := strings.TrimSpace(text[:separator])
		declID := strings.TrimSpace(text[separator+1:])
		if modulePath == "" || declID == "" {
			return TypeRef{}, fmt.Errorf("malformed qualified type %q", text)
		}
		return TypeRef{Kind: Named, Named: TypeKey{ModulePath: modulePath, DeclID: DeclID(declID)}}, nil
	}
	return TypeRef{Kind: Named, Named: TypeKey{ModulePath: p.ModulePath, DeclID: DeclID(text)}}, nil
}

// ParseCanonical parses a compiler or wire boundary type and requires every
// named identity to exist in the parser's type table. Source names must be
// resolved by semantic analysis before reaching this entry point.
func (p *Parser) ParseCanonical(text string) (TypeRef, error) {
	ref, err := p.Parse(text)
	if err != nil {
		return TypeRef{}, err
	}
	if err := p.validateCanonicalRef(ref, make(map[TypeID]bool)); err != nil {
		return TypeRef{}, err
	}
	return ref, nil
}

func (p *Parser) validateCanonicalRef(ref TypeRef, seen map[TypeID]bool) error {
	if ref.Kind == Named {
		if _, ok := p.Table.Named(ref.Named); !ok {
			return fmt.Errorf("unknown canonical named type %s.%s", ref.Named.ModulePath, ref.Named.DeclID)
		}
	}
	if ref.Node == "" || seen[ref.Node] {
		return nil
	}
	node, ok := p.Table.Node(ref)
	if !ok {
		return fmt.Errorf("unknown canonical type node %q", ref.Node)
	}
	seen[ref.Node] = true
	defer delete(seen, ref.Node)
	for _, child := range nodeRefs(node) {
		if child.Valid() {
			if err := p.validateCanonicalRef(child, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Parser) node(node TypeNode, source string) (TypeRef, error) {
	if len(p.Bindings) != 0 {
		names := make([]string, 0, len(p.Bindings))
		for name := range p.Bindings {
			names = append(names, name)
		}
		sort.Strings(names)
		var identity strings.Builder
		identity.WriteString(source)
		for _, name := range names {
			ref := p.Bindings[name]
			identity.WriteByte(0)
			identity.WriteString(name)
			identity.WriteByte(':')
			fmt.Fprintf(&identity, "%v", ref)
		}
		source = identity.String()
	}
	hash := sha256.Sum256([]byte(source))
	node.ID = TypeID("anon." + hex.EncodeToString(hash[:8]))
	if _, exists := p.Table.Node(TypeRef{Node: node.ID}); exists {
		return TypeRef{Kind: node.Kind, Node: node.ID}, nil
	}
	if err := p.Table.Add(node); err != nil {
		return TypeRef{}, err
	}
	return TypeRef{Kind: node.Kind, Node: node.ID}, nil
}

func (p *Parser) parseFunction(text string) (FunctionSignature, error) {
	closeIndex := matchingClose(text, strings.IndexByte(text, '('))
	if closeIndex < 0 {
		return FunctionSignature{}, fmt.Errorf("malformed function type %q", text)
	}
	var sig FunctionSignature
	inner := strings.TrimSpace(text[len("function("):closeIndex])
	if inner != "" {
		parts := splitTopLevel(inner, ',')
		for i, part := range parts {
			part = strings.TrimSpace(part)
			variadic := strings.HasPrefix(part, "variadic ")
			if variadic {
				part = strings.TrimSpace(strings.TrimPrefix(part, "variadic "))
			}
			ref, err := p.Parse(part)
			if err != nil {
				return FunctionSignature{}, err
			}
			sig.Params = append(sig.Params, TypeParam{Type: ref})
			if variadic {
				if i != len(parts)-1 || ref.Kind != Slice {
					return FunctionSignature{}, errors.New("invalid variadic function parameter")
				}
				sig.Variadic = true
			}
		}
	}
	result := strings.TrimSpace(text[closeIndex+1:])
	if result == "" || result == "Void" {
		return sig, nil
	}
	if strings.HasPrefix(result, "tuple(") && strings.HasSuffix(result, ")") {
		result = strings.TrimSpace(result[len("tuple(") : len(result)-1])
		if result != "" {
			for _, part := range splitTopLevel(result, ',') {
				ref, err := p.Parse(part)
				if err != nil {
					return FunctionSignature{}, err
				}
				sig.Results = append(sig.Results, ref)
			}
		}
		return sig, nil
	}
	ref, err := p.Parse(result)
	if err != nil {
		return FunctionSignature{}, err
	}
	sig.Results = []TypeRef{ref}
	return sig, nil
}

func (p *Parser) parseStruct(text string) (TypeRef, error) {
	inner := strings.TrimSpace(text[len("struct{") : len(text)-1])
	node := TypeNode{Kind: Struct}
	for _, part := range splitTopLevel(inner, ',') {
		if strings.TrimSpace(part) == "" {
			continue
		}
		name, fieldType, tag, embedded, valid := splitStructFieldText(part)
		if !valid {
			return TypeRef{}, fmt.Errorf("malformed struct field %q", part)
		}
		ref, err := p.Parse(fieldType)
		if err != nil {
			return TypeRef{}, err
		}
		node.Fields = append(node.Fields, Field{Name: name, Type: ref, Tag: tag, Embedded: embedded})
	}
	return p.node(node, text)
}

func (p *Parser) parseInterface(text string) (TypeRef, error) {
	inner := strings.TrimSpace(text[len("interface{") : len(text)-1])
	node := TypeNode{Kind: Interface}
	for _, part := range splitTopLevel(inner, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, ":") {
			parts := splitTopLevel(part, ':')
			if len(parts) != 2 {
				return TypeRef{}, fmt.Errorf("malformed interface method %q", part)
			}
			signature, err := p.parseFunction(strings.TrimSpace(parts[1]))
			if err != nil {
				return TypeRef{}, err
			}
			node.Methods = append(node.Methods, Method{Name: strings.TrimSpace(parts[0]), Signature: signature, ModulePath: p.ModulePath})
			continue
		}
		approx := strings.HasPrefix(part, "~")
		if approx {
			part = strings.TrimSpace(strings.TrimPrefix(part, "~"))
		}
		parts := splitTopLevel(part, '|')
		if len(parts) > 1 {
			node.TypeSet = true
		}
		for i, term := range parts {
			ref, err := p.Parse(strings.TrimSpace(strings.TrimPrefix(term, "~")))
			if err != nil {
				return TypeRef{}, err
			}
			node.Terms = append(node.Terms, TypeTerm{Type: ref, Approx: approx && i == 0, Union: len(parts) > 1})
		}
	}
	return p.node(node, text)
}

func primitiveByName(name string) PrimitiveKind {
	switch name {
	case "Bool":
		return PrimitiveBool
	case "String":
		return PrimitiveString
	case "Int":
		return PrimitiveInt
	case "Int8":
		return PrimitiveInt8
	case "Int16":
		return PrimitiveInt16
	case "Int32":
		return PrimitiveInt32
	case "Int64":
		return PrimitiveInt64
	case "Uint":
		return PrimitiveUint
	case "Uint8":
		return PrimitiveUint8
	case "Uint16":
		return PrimitiveUint16
	case "Uint32":
		return PrimitiveUint32
	case "Uint64":
		return PrimitiveUint64
	case "Uintptr":
		return PrimitiveUintptr
	case "Float32":
		return PrimitiveFloat32
	case "Float64":
		return PrimitiveFloat64
	case "Complex64":
		return PrimitiveComplex64
	case "Complex128":
		return PrimitiveComplex128
	case "Error":
		return PrimitiveError
	case "Function":
		return PrimitiveFunction
	default:
		return PrimitiveInvalid
	}
}

func matchingClose(text string, open int) int {
	if open < 0 {
		return -1
	}
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitTopLevel(text string, separator byte) []string {
	var out []string
	start, depth := 0, 0
	inTag := false
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '`':
			inTag = !inTag
		case '<', '(', '{':
			if !inTag {
				depth++
			}
		case '>', ')', '}':
			if !inTag && depth > 0 {
				depth--
			}
		default:
			if !inTag && depth == 0 && text[i] == separator {
				out = append(out, text[start:i])
				start = i + 1
			}
		}
	}
	return append(out, text[start:])
}
