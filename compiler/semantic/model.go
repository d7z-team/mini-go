// Package semantic owns source-level name and type information used by lowering.
package semantic

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
	bytecode "github.com/d7z-team/mini-go/runtime/bytecode"
)

type (
	ObjectID string
	ScopeID  uint32
)

type ObjectKind uint8

const (
	ObjectInvalid ObjectKind = iota
	ObjectBuiltin
	ObjectImport
	ObjectConst
	ObjectVar
	ObjectType
	ObjectFunc
	ObjectField
	ObjectLabel
	ObjectTypeParam
)

func (kind ObjectKind) dependencyKind() bool {
	return kind == ObjectConst || kind == ObjectVar || kind == ObjectType || kind == ObjectFunc
}

type Object struct {
	ID         ObjectID
	Kind       ObjectKind
	Name       string
	Node       ast.NodeID
	Scope      ScopeID
	Type       types.TypeRef
	ImportPath string
	ModulePath string
	ExportName string
	FunctionID string
	Exported   bool
	Blank      bool
	Alias      bool
	Untyped    bool
	Mutable    bool
	Definition ast.Identifier
}

type ScopeKind uint8

const (
	ScopeInvalid ScopeKind = iota
	ScopeUniverse
	ScopePackage
	ScopeFile
	ScopeFunction
	ScopeBlock
	ScopeClause
	ScopeType
)

type Scope struct {
	ID      ScopeID
	Kind    ScopeKind
	Node    ast.NodeID
	Parent  ScopeID
	Objects map[string]ObjectID
}

type ExprMode uint8

const (
	ExprInvalid ExprMode = iota
	ExprValue
	ExprConstant
	ExprType
	ExprBuiltin
	ExprPackage
	ExprNoValue
	ExprMultiValue
)

type ValueCategory uint8

const (
	ValueInvalid ValueCategory = iota
	Value
	ValueAddressable
	ValueMapIndex
	ValueBlank
)

func (category ValueCategory) Addressable() bool { return category == ValueAddressable }

func (category ValueCategory) Assignable() bool {
	return category == ValueAddressable || category == ValueMapIndex || category == ValueBlank
}

type ExprInfo struct {
	Type        types.TypeRef
	Mode        ExprMode
	Untyped     bool
	Category    ValueCategory
	LeftTarget  types.TypeRef
	RightTarget types.TypeRef
	Results     []types.TypeRef
	Object      ObjectID
	Signature   *types.FunctionSignature
}

type CompositeInfo struct {
	Type          types.TypeRef
	TypeExact     bool
	Shape         types.Kind
	Key           types.TypeRef
	Element       types.TypeRef
	Fields        []CompositeField
	Initializers  []Selection
	ModulePath    string
	InferredArray bool
}

type CompositeField struct {
	Name     string
	Type     types.TypeRef
	Tag      string
	Embedded bool
	Variadic bool
}

// GenericMethod is a source declaration, not a runtime method-set entry.
type GenericMethod struct {
	Method types.Method
	Object ObjectID
}

type EmbedKind uint8

const (
	EmbedInvalid EmbedKind = iota
	EmbedString
	EmbedBytes
	EmbedFS
)

type EmbedInfo struct {
	Kind    EmbedKind
	Type    types.TypeRef
	Storage types.TypeRef
}

type SwitchKind uint8

const (
	SwitchInvalid SwitchKind = iota
	SwitchExpression
	SwitchType
)

type SwitchCaseInfo struct {
	Node       ast.NodeID
	Types      []types.TypeRef
	Binding    types.TypeRef
	Implements []bool
	TypeSets   []bool
	Nil        bool
	Default    bool
}

type SwitchInfo struct {
	Kind       SwitchKind
	Tag        types.TypeRef
	Subject    types.TypeRef
	Comparable bool
	Interface  bool
	TypeSet    bool
	Cases      []SwitchCaseInfo
}

type TypeInfo struct {
	Type types.TypeRef
}

type SelectionKind uint8

const (
	SelectionInvalid SelectionKind = iota
	SelectionPackageMember
	SelectionField
	SelectionMethod
	SelectionMethodExpression
)

type Selection struct {
	Kind              SelectionKind
	Object            ObjectID
	Name              string
	ModulePath        string
	FunctionID        string
	Receiver          types.TypeRef
	DeclaringReceiver types.TypeRef
	Type              types.TypeRef
	Signature         types.FunctionSignature
	Variadic          bool
	Interface         bool
	Indirect          bool
	Index             []int
}

// OperatorSelection records a source operator lowered as an ordinary method
// call after the built-in operation has been ruled out.
type OperatorSelection struct {
	Operator  string
	Selection Selection
}

type CallKind uint8

const (
	CallInvalid CallKind = iota
	CallFunction
	CallBuiltin
	CallConversion
	CallMethod
	CallInterfaceMethod
	CallIntrinsic
)

type CallInfo struct {
	Kind      CallKind
	Callee    ObjectID
	Target    types.TypeRef
	Signature types.FunctionSignature
	Variadic  bool
	TypeArgs  []types.TypeRef
}

type InstanceInfo struct {
	Generic  ObjectID
	TypeArgs []types.TypeRef
}

type ProgramInfo struct {
	ModulePath     string
	Package        string
	TypeTable      *types.TypeTable
	Relations      types.Relations
	Constants      map[ast.NodeID]constant.Value
	ConstObjects   map[ObjectID]constant.Value
	ArrayLengths   map[ast.NodeID]int64
	Universe       ScopeID
	PackageScope   ScopeID
	Scopes         map[ScopeID]*Scope
	NodeScopes     map[ast.NodeID]ScopeID
	Objects        map[ObjectID]Object
	Defs           map[ast.NodeID][]ObjectID
	Uses           map[ast.NodeID]ObjectID
	NameDefs       map[ast.NameID]ObjectID
	NameUses       map[ast.NameID]ObjectID
	Exprs          map[ast.NodeID]ExprInfo
	Types          map[ast.NodeID]TypeInfo
	Calls          map[ast.NodeID]CallInfo
	Instances      map[ast.NodeID]InstanceInfo
	Selections     map[ast.NodeID]Selection
	Operators      map[ast.NodeID]OperatorSelection
	Composites     map[ast.NodeID]CompositeInfo
	Embeds         map[ast.NodeID]EmbedInfo
	Switches       map[ast.NodeID]SwitchInfo
	IntrinsicCalls map[ast.NodeID]bytecode.IntrinsicID
	Diagnostics    []source.Diagnostic
	GenericDecls   map[ObjectID][]ObjectID
	GenericMethods []GenericMethod
	Functions      map[ast.NodeID]ObjectID
}

// CheckedProgram is the immutable boundary between source semantic analysis
// and emit. The Program and Info always share the same assigned node IDs.
type CheckedProgram struct {
	Program ast.Program
	Info    *ProgramInfo
}

func (p *ProgramInfo) Scope(id ScopeID) *Scope {
	if p == nil {
		return nil
	}
	return p.Scopes[id]
}

func (p *ProgramInfo) Object(id ObjectID) (Object, bool) {
	if p == nil {
		return Object{}, false
	}
	object, ok := p.Objects[id]
	return object, ok
}

// TypeExact reports whether semantic analysis has resolved every value needed
// to format the type.
func (p *ProgramInfo) TypeExact(ref types.TypeRef) bool {
	return p != nil && p.TypeTable.TypeExact(ref)
}

func (p *ProgramInfo) Constant(node ast.NodeID) (constant.Value, bool) {
	if p == nil {
		return constant.Value{}, false
	}
	value, ok := p.Constants[node]
	return value, ok
}

func (p *ProgramInfo) Lookup(scopeID ScopeID, name string) (Object, bool) {
	if p == nil {
		return Object{}, false
	}
	for scopeID != 0 {
		scope := p.Scopes[scopeID]
		if scope == nil {
			break
		}
		if id, ok := scope.Objects[name]; ok {
			return p.Object(id)
		}
		scopeID = scope.Parent
	}
	return Object{}, false
}
