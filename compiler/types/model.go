// Package types defines the language-neutral canonical type algebra.
package types

import "sync"

type Kind uint8

const (
	Invalid Kind = iota
	Void
	Any
	Primitive
	Named
	Slice
	Array
	Map
	Pointer
	Waitable
	Function
	Tuple
	Struct
	Interface
	TypeParameter
	Instance
)

type PrimitiveKind uint8

const (
	PrimitiveInvalid PrimitiveKind = iota
	PrimitiveBool
	PrimitiveString
	PrimitiveInt
	PrimitiveInt8
	PrimitiveInt16
	PrimitiveInt32
	PrimitiveInt64
	PrimitiveUint
	PrimitiveUint8
	PrimitiveUint16
	PrimitiveUint32
	PrimitiveUint64
	PrimitiveUintptr
	PrimitiveFloat32
	PrimitiveFloat64
	PrimitiveComplex64
	PrimitiveComplex128
	PrimitiveError
	PrimitiveFunction
)

type ChannelDir uint8

const (
	ChannelInvalid ChannelDir = iota
	ChannelBoth
	ChannelReceive
	ChannelSend
)

type (
	TypeID string
	DeclID string
)

type TypeKey struct {
	ModulePath string `json:"module_path"`
	DeclID     DeclID `json:"decl_id"`
}

type TypeRef struct {
	Kind      Kind          `json:"kind"`
	Primitive PrimitiveKind `json:"primitive,omitempty"`
	Named     TypeKey       `json:"named,omitempty"`
	Node      TypeID        `json:"node,omitempty"`
}

func (r TypeRef) Valid() bool {
	switch r.Kind {
	case Void, Any:
		return r.Primitive == PrimitiveInvalid && emptyTypeKey(r.Named) && r.Node == ""
	case Primitive:
		return r.Primitive != PrimitiveInvalid && emptyTypeKey(r.Named) && r.Node == ""
	case Named:
		return (r.Named.ModulePath != "" && r.Named.DeclID != "") || r.Node != ""
	default:
		return r.Node != "" && r.Primitive == PrimitiveInvalid && emptyTypeKey(r.Named)
	}
}

func emptyTypeKey(key TypeKey) bool {
	return key.ModulePath == "" && key.DeclID == ""
}

func (r TypeRef) Equal(other TypeRef) bool {
	return r == other
}

func Builtin(kind PrimitiveKind) TypeRef {
	if kind == PrimitiveInvalid {
		return TypeRef{}
	}
	return TypeRef{Kind: Primitive, Primitive: kind}
}

func VoidType() TypeRef { return TypeRef{Kind: Void} }
func AnyType() TypeRef  { return TypeRef{Kind: Any} }

type TypeParam struct {
	Type TypeRef `json:"type"`
}

type FunctionSignature struct {
	Params   []TypeParam `json:"params,omitempty"`
	Results  []TypeRef   `json:"results,omitempty"`
	Variadic bool        `json:"variadic,omitempty"`
}

type Field struct {
	Name     string  `json:"name"`
	Type     TypeRef `json:"type"`
	Tag      string  `json:"tag,omitempty"`
	Embedded bool    `json:"embedded,omitempty"`
}

type Method struct {
	Name       string            `json:"name"`
	Receiver   TypeRef           `json:"receiver"`
	Signature  FunctionSignature `json:"signature"`
	FunctionID string            `json:"function_id,omitempty"`
	ModulePath string            `json:"module_path,omitempty"`
}

type TypeTerm struct {
	Type   TypeRef `json:"type"`
	Approx bool    `json:"approx,omitempty"`
	Union  bool    `json:"union,omitempty"`
}

type TypeNode struct {
	ID          TypeID             `json:"id"`
	Kind        Kind               `json:"kind"`
	Name        string             `json:"name,omitempty"`
	Primitive   PrimitiveKind      `json:"primitive,omitempty"`
	Identity    TypeKey            `json:"identity,omitempty"`
	Alias       bool               `json:"alias,omitempty"`
	AliasTarget TypeRef            `json:"alias_target,omitempty"`
	Underlying  TypeRef            `json:"underlying,omitempty"`
	Elem        TypeRef            `json:"elem,omitempty"`
	Key         TypeRef            `json:"key,omitempty"`
	Length      int64              `json:"length,omitempty"`
	Direction   ChannelDir         `json:"direction,omitempty"`
	Signature   *FunctionSignature `json:"signature,omitempty"`
	Tuple       []TypeRef          `json:"tuple,omitempty"`
	Fields      []Field            `json:"fields,omitempty"`
	Methods     []Method           `json:"methods,omitempty"`
	Terms       []TypeTerm         `json:"terms,omitempty"`
	TypeSet     bool               `json:"type_set,omitempty"`
	Constraint  TypeRef            `json:"constraint,omitempty"`
	Base        TypeRef            `json:"base,omitempty"`
	TypeArgs    []TypeRef          `json:"type_args,omitempty"`
}

// UnknownArrayLength is a compiler-only recovery value. Runtime artifacts
// must contain a non-negative, fully resolved array length.
const UnknownArrayLength int64 = -1

type TypeTable struct {
	Nodes []TypeNode `json:"nodes,omitempty"`

	byID       map[TypeID]int
	byName     map[TypeKey]TypeID
	cache      *typeTableCache
	indexOwner *TypeTable
}

type typeTableCache struct {
	sync.RWMutex
	underlying map[TypeRef]TypeRef
	formatted  map[TypeRef]string
}
