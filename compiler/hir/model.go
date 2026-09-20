// Package hir defines the typed, source-independent input to bytecode emission.
package hir

import (
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler/types"
)

type Program struct {
	ModulePath      string
	Package         string
	TypeTable       types.TypeTable
	DebugSourceHash string
	DebugFiles      []SourceFile
	Constants       []Constant
	Globals         []Global
	Functions       []Function
	Exports         []Export
	Requirements    []Requirement
}

type SourceFile struct {
	ID   string
	Path string
	Hash string
}

type Location struct {
	File   string
	Line   int
	Column int
}

type Constant struct {
	ID      string
	Name    string
	Type    types.TypeRef
	Value   json.RawMessage
	Untyped bool
}

type Global struct {
	ID   string
	Name string
	Type types.TypeRef
}

type Function struct {
	ID            string
	Name          string
	RevisionLocal bool
	Generated     bool
	Declaration   *Location
	Signature     types.FunctionSignature
	Locals        []Local
	ResultLocals  []string
	Upvalues      []Upvalue
	DebugScopes   []DebugScope
	Body          []Statement
}

type Local struct {
	ID          string
	Name        string
	Type        types.TypeRef
	Scope       int
	Generated   bool
	Declaration *Location
}

type DebugScope struct {
	ID     int
	Parent int
}

type Upvalue struct {
	ID   string
	Name string
	Type types.TypeRef
}

type Export struct {
	Name    string
	Kind    string
	ID      string
	Type    types.TypeRef
	Untyped bool
}

type Requirement struct {
	Kind       string
	ModulePath string
	Hash       string
	Exports    []string
}

type StatementKind string

const (
	StmtExpr           StatementKind = "expr"
	StmtMapIterInit    StatementKind = "map_iter_init"
	StmtMapIterClose   StatementKind = "map_iter_close"
	StmtReturn         StatementKind = "return"
	StmtTailCallDirect StatementKind = "tail_call_direct"
	StmtStoreLocal     StatementKind = "store_local"
	StmtStoreUpvalue   StatementKind = "store_upvalue"
	StmtStoreGlobal    StatementKind = "store_global"
	StmtStoreResults   StatementKind = "store_results"
	StmtStoreValues    StatementKind = "store_values"
	StmtStoreIndex     StatementKind = "store_index"
	StmtStoreField     StatementKind = "store_field"
	StmtStoreIndirect  StatementKind = "store_indirect"
	StmtChanSend       StatementKind = "chan_send"
	StmtSelect         StatementKind = "select"
	StmtSpawn          StatementKind = "spawn"
	StmtInitModule     StatementKind = "init_module"
	StmtPanic          StatementKind = "panic"
	StmtDefer          StatementKind = "defer"
	StmtLabel          StatementKind = "label"
	StmtJump           StatementKind = "jump"
	StmtJumpIf         StatementKind = "jump_if"
)

type Statement struct {
	Kind         StatementKind
	SourcePoints []Location
	Scope        int
	Expr         Expression
	Results      []Expression
	Values       []Expression
	Args         []Expression
	Object       Expression
	Index        Expression
	Local        string
	Rebind       bool
	Upvalue      string
	Global       string
	Module       string
	Field        string
	Label        string
	// DeferOwnerDepth selects the caller frame that owns a deferred call.
	// Zero means the current frame.
	DeferOwnerDepth int
	Targets         []StoreTarget
	SelectCases     []SelectCase
	SelectDefault   bool
}

// SelectCase refers to already evaluated local operands. Receive destinations
// are private temporaries; source assignment targets run after selection.
type SelectCase struct {
	Channel string
	Send    string
	Value   string
	OK      string
}

type ExpressionKind string

const (
	ExprLiteral             ExpressionKind = "literal"
	ExprConst               ExpressionKind = "const"
	ExprZero                ExpressionKind = "zero"
	ExprLocal               ExpressionKind = "local"
	ExprUpvalue             ExpressionKind = "upvalue"
	ExprGlobal              ExpressionKind = "global"
	ExprLet                 ExpressionKind = "let"
	ExprLetResults          ExpressionKind = "let_results"
	ExprValues              ExpressionKind = "values"
	ExprUnary               ExpressionKind = "unary"
	ExprBinary              ExpressionKind = "binary"
	ExprCallDirect          ExpressionKind = "call_direct"
	ExprCallFFI             ExpressionKind = "call_ffi"
	ExprCallIntrinsic       ExpressionKind = "call_intrinsic"
	ExprCallValue           ExpressionKind = "call_value"
	ExprCallInterface       ExpressionKind = "call_interface"
	ExprFunction            ExpressionKind = "function"
	ExprLoadExport          ExpressionKind = "load_export"
	ExprSequence            ExpressionKind = "sequence"
	ExprMap                 ExpressionKind = "map"
	ExprStruct              ExpressionKind = "struct"
	ExprMakeSlice           ExpressionKind = "make_slice"
	ExprMakeChan            ExpressionKind = "make_chan"
	ExprLoadIndex           ExpressionKind = "load_index"
	ExprLoadIndexOK         ExpressionKind = "load_index_ok"
	ExprStringRuneAt        ExpressionKind = "string_rune_at"
	ExprStringNextRuneIndex ExpressionKind = "string_next_rune_index"
	ExprSlice               ExpressionKind = "slice"
	ExprLen                 ExpressionKind = "len"
	ExprCap                 ExpressionKind = "cap"
	ExprAppend              ExpressionKind = "append"
	ExprDelete              ExpressionKind = "delete"
	ExprClear               ExpressionKind = "clear"
	ExprCopy                ExpressionKind = "copy"
	ExprMapKeys             ExpressionKind = "map_keys"
	ExprMapIterNext         ExpressionKind = "map_iter_next"
	ExprLoadField           ExpressionKind = "load_field"
	ExprTypeAssert          ExpressionKind = "type_assert"
	ExprTypeAssertOK        ExpressionKind = "type_assert_ok"
	ExprConvert             ExpressionKind = "convert"
	ExprAddressOf           ExpressionKind = "address_of"
	ExprLoadIndirect        ExpressionKind = "load_indirect"
	ExprChanRecv            ExpressionKind = "chan_recv"
	ExprChanRecvOK          ExpressionKind = "chan_recv_ok"
	ExprChanCanRecv         ExpressionKind = "chan_can_recv"
	ExprChanTryRecv         ExpressionKind = "chan_try_recv"
	ExprChanTrySend         ExpressionKind = "chan_try_send"
	ExprChanCanSend         ExpressionKind = "chan_can_send"
	ExprChanClose           ExpressionKind = "chan_close"
	ExprRecover             ExpressionKind = "recover"
)

type Expression struct {
	Kind        ExpressionKind
	Type        types.TypeRef
	Untyped     bool
	ConstantID  string
	Value       json.RawMessage
	Local       string
	Locals      []string
	Upvalue     string
	Global      string
	Operator    string
	Left        *Expression
	Right       *Expression
	Operand     *Expression
	Bind        *Expression
	Body        *Expression
	Function    string
	Intrinsic   string
	ModulePath  string
	Export      string
	Args        []Expression
	Ellipsis    bool
	Captures    []CaptureTarget
	ResultCount int
	Elements    []Expression
	Entries     []MapEntry
	Fields      []FieldValue
	Size        *Expression
	Index       *Expression
	Start       *Expression
	End         *Expression
	Max         *Expression
	Field       string
	Path        []AddressSegment
}

type AddressSegment struct {
	Kind  string
	Field string
	Local string
}

type CaptureTarget struct {
	Kind    string
	Local   string
	Upvalue string
	Global  string
}

type StoreTarget struct {
	Kind    string
	Local   string
	Rebind  bool
	Upvalue string
	Global  string
}

type MapEntry struct {
	Key   Expression
	Value Expression
}

type FieldValue struct {
	Name  string
	Value Expression
}
