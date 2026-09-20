package ast

import "github.com/d7z-team/mini-go/compiler/source"

type (
	NodeID uint64
	NameID uint64
)

type Identifier struct {
	ID   NameID      `json:"id,omitempty"`
	Text string      `json:"text,omitempty"`
	Span source.Span `json:"span,omitempty"`
}

type Program struct {
	NodeID     NodeID     `json:"node_id,omitempty"`
	ModulePath string     `json:"module_path"`
	Package    string     `json:"package"`
	PackageID  Identifier `json:"package_identifier,omitempty"`
	Files      []File     `json:"files,omitempty"`
}

type File struct {
	NodeID    NodeID      `json:"node_id,omitempty"`
	ID        string      `json:"id,omitempty"`
	Path      string      `json:"path"`
	Hash      string      `json:"hash,omitempty"`
	PackageID Identifier  `json:"package_identifier,omitempty"`
	Span      source.Span `json:"span,omitempty"`
	Decls     []Decl      `json:"decls,omitempty"`
}

type DeclKind string

const (
	DeclInvalid DeclKind = "invalid"
	DeclImport  DeclKind = "import"
	DeclConst   DeclKind = "const"
	DeclVar     DeclKind = "var"
	DeclType    DeclKind = "type"
	DeclFunc    DeclKind = "func"
)

type Decl struct {
	NodeID NodeID      `json:"node_id,omitempty"`
	Kind   DeclKind    `json:"kind"`
	Span   source.Span `json:"span,omitempty"`
	Import ImportDecl  `json:"import,omitempty"`
	Const  ValueDecl   `json:"const,omitempty"`
	Var    ValueDecl   `json:"var,omitempty"`
	Type   TypeDecl    `json:"type,omitempty"`
	Func   FuncDecl    `json:"func,omitempty"`
}

type ImportDecl struct {
	Path     string      `json:"path"`
	PathSpan source.Span `json:"path_span,omitempty"`
	Alias    string      `json:"alias,omitempty"`
	AliasID  Identifier  `json:"alias_identifier,omitempty"`
}

// IsEmbedMarker reports whether the import only enables //go:embed directives.
func (decl ImportDecl) IsEmbedMarker() bool {
	return decl.Alias == "_" && decl.Path == "embed"
}

type ValueDecl struct {
	Names         []string     `json:"names,omitempty"`
	NameIDs       []Identifier `json:"name_identifiers,omitempty"`
	Type          TypeExpr     `json:"type,omitempty"`
	Values        []Expression `json:"values,omitempty"`
	EmbedPatterns []string     `json:"embed_patterns,omitempty"`
}

// EmbedFile is one package-relative regular file selected by //go:embed.
// It is compiler input carried only until the embed initializer is lowered.
type EmbedFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

type TypeDecl struct {
	Name       string      `json:"name"`
	NameID     Identifier  `json:"name_identifier,omitempty"`
	TypeParams []TypeParam `json:"type_params,omitempty"`
	Type       TypeExpr    `json:"type"`
	Alias      bool        `json:"alias,omitempty"`
}

type FuncDecl struct {
	NodeID     NodeID      `json:"node_id,omitempty"`
	Name       string      `json:"name"`
	NameID     Identifier  `json:"name_identifier,omitempty"`
	Receiver   *Field      `json:"receiver,omitempty"`
	TypeParams []TypeParam `json:"type_params,omitempty"`
	Params     []Field     `json:"params,omitempty"`
	Results    []Field     `json:"results,omitempty"`
	Body       BlockStmt   `json:"body,omitempty"`
	Template   bool        `json:"template,omitempty"`
}

type TypeParam struct {
	NodeID     NodeID      `json:"node_id,omitempty"`
	Name       string      `json:"name"`
	NameID     Identifier  `json:"name_identifier,omitempty"`
	Constraint TypeExpr    `json:"constraint"`
	Span       source.Span `json:"span,omitempty"`
}

type Field struct {
	NodeID   NodeID      `json:"node_id,omitempty"`
	Name     string      `json:"name,omitempty"`
	NameID   Identifier  `json:"name_identifier,omitempty"`
	Type     TypeExpr    `json:"type"`
	Tag      string      `json:"tag,omitempty"`
	Variadic bool        `json:"variadic,omitempty"`
	Span     source.Span `json:"span,omitempty"`
}

type TypeKind string

const (
	TypeInvalid   TypeKind = ""
	TypeName      TypeKind = "name"
	TypeArray     TypeKind = "array"
	TypeSlice     TypeKind = "slice"
	TypeMap       TypeKind = "map"
	TypePointer   TypeKind = "pointer"
	TypeFunc      TypeKind = "func"
	TypeStruct    TypeKind = "struct"
	TypeInterface TypeKind = "interface"
	TypeChan      TypeKind = "chan"
	TypeInstance  TypeKind = "instance"
)

type TypeExpr struct {
	NodeID      NodeID      `json:"node_id,omitempty"`
	Kind        TypeKind    `json:"kind,omitempty"`
	Name        string      `json:"name,omitempty"`
	NameID      Identifier  `json:"name_identifier,omitempty"`
	QualifierID Identifier  `json:"qualifier_identifier,omitempty"`
	Elem        *TypeExpr   `json:"elem,omitempty"`
	Key         *TypeExpr   `json:"key,omitempty"`
	Len         *Expression `json:"len,omitempty"`
	LenInfer    bool        `json:"len_infer,omitempty"`
	Params      []Field     `json:"params,omitempty"`
	Results     []Field     `json:"results,omitempty"`
	Fields      []Field     `json:"fields,omitempty"`
	Methods     []FuncDecl  `json:"methods,omitempty"`
	Embeds      []TypeExpr  `json:"embeds,omitempty"`
	Terms       []TypeTerm  `json:"terms,omitempty"`
	Direction   string      `json:"direction,omitempty"`
	Base        *TypeExpr   `json:"base,omitempty"`
	TypeArgs    []TypeExpr  `json:"type_args,omitempty"`
	Span        source.Span `json:"span,omitempty"`
}

type TypeTerm struct {
	NodeID NodeID      `json:"node_id,omitempty"`
	Type   TypeExpr    `json:"type"`
	Approx bool        `json:"approx,omitempty"`
	Span   source.Span `json:"span,omitempty"`
}

type StmtKind string

const (
	StmtInvalid StmtKind = "invalid"
	StmtEmpty   StmtKind = "empty"
	StmtDecl    StmtKind = "decl"
	StmtExpr    StmtKind = "expr"
	StmtAssign  StmtKind = "assign"
	StmtReturn  StmtKind = "return"
	StmtIf      StmtKind = "if"
	StmtFor     StmtKind = "for"
	StmtRange   StmtKind = "range"
	StmtSwitch  StmtKind = "switch"
	StmtSelect  StmtKind = "select"
	StmtSend    StmtKind = "send"
	StmtBlock   StmtKind = "block"
	StmtLabel   StmtKind = "label"
	StmtBranch  StmtKind = "branch"
	StmtDefer   StmtKind = "defer"
	StmtGo      StmtKind = "go"
	StmtPanic   StmtKind = "panic"
)

type BlockStmt struct {
	NodeID NodeID      `json:"node_id,omitempty"`
	Span   source.Span `json:"span,omitempty"`
	Stmts  []Statement `json:"stmts,omitempty"`
}

type Statement struct {
	NodeID  NodeID       `json:"node_id,omitempty"`
	Kind    StmtKind     `json:"kind"`
	Span    source.Span  `json:"span,omitempty"`
	Decls   []Decl       `json:"decls,omitempty"`
	Expr    *Expression  `json:"expr,omitempty"`
	Left    []Expression `json:"left,omitempty"`
	Right   []Expression `json:"right,omitempty"`
	Op      string       `json:"op,omitempty"`
	Body    BlockStmt    `json:"body,omitempty"`
	Init    *Statement   `json:"init,omitempty"`
	Cond    *Expression  `json:"cond,omitempty"`
	Post    *Statement   `json:"post,omitempty"`
	Else    *Statement   `json:"else,omitempty"`
	Key     *Expression  `json:"key,omitempty"`
	Value   *Expression  `json:"value,omitempty"`
	Range   *Expression  `json:"range,omitempty"`
	Cases   []CaseClause `json:"cases,omitempty"`
	Label   string       `json:"label,omitempty"`
	LabelID Identifier   `json:"label_identifier,omitempty"`
	Results []Expression `json:"results,omitempty"`
	// TypeSwitch marks a source type switch guard. TypeSwitchName is optional.
	TypeSwitch     bool       `json:"type_switch,omitempty"`
	TypeSwitchName string     `json:"type_switch_name,omitempty"`
	TypeSwitchID   Identifier `json:"type_switch_identifier,omitempty"`
}

type CaseClause struct {
	NodeID  NodeID       `json:"node_id,omitempty"`
	Span    source.Span  `json:"span,omitempty"`
	Values  []Expression `json:"values,omitempty"`
	Types   []TypeExpr   `json:"types,omitempty"`
	Nil     bool         `json:"nil,omitempty"`
	Comm    *Statement   `json:"comm,omitempty"`
	Body    BlockStmt    `json:"body,omitempty"`
	Default bool         `json:"default,omitempty"`
}

type ExprKind string

const (
	ExprInvalid   ExprKind = ""
	ExprIdent     ExprKind = "ident"
	ExprLiteral   ExprKind = "literal"
	ExprUnary     ExprKind = "unary"
	ExprBinary    ExprKind = "binary"
	ExprCall      ExprKind = "call"
	ExprSelector  ExprKind = "selector"
	ExprIndex     ExprKind = "index"
	ExprIndexList ExprKind = "index_list"
	ExprSlice     ExprKind = "slice"
	ExprComposite ExprKind = "composite"
	ExprFunc      ExprKind = "func"
	ExprConvert   ExprKind = "convert"
	ExprAssert    ExprKind = "assert"
	ExprAddr      ExprKind = "addr"
	ExprDeref     ExprKind = "deref"
	ExprReceive   ExprKind = "receive"
	ExprEmbed     ExprKind = "embed"
)

type Expression struct {
	NodeID     NodeID       `json:"node_id,omitempty"`
	Kind       ExprKind     `json:"kind,omitempty"`
	Span       source.Span  `json:"span,omitempty"`
	Name       string       `json:"name,omitempty"`
	NameID     Identifier   `json:"name_identifier,omitempty"`
	Literal    string       `json:"literal,omitempty"`
	Type       TypeExpr     `json:"type,omitempty"`
	TypeSwitch bool         `json:"type_switch,omitempty"`
	Operator   string       `json:"operator,omitempty"`
	Left       *Expression  `json:"left,omitempty"`
	Right      *Expression  `json:"right,omitempty"`
	Operand    *Expression  `json:"operand,omitempty"`
	Callee     *Expression  `json:"callee,omitempty"`
	Args       []Expression `json:"args,omitempty"`
	Ellipsis   bool         `json:"ellipsis,omitempty"`
	Field      string       `json:"field,omitempty"`
	FieldID    Identifier   `json:"field_identifier,omitempty"`
	Index      *Expression  `json:"index,omitempty"`
	Start      *Expression  `json:"start,omitempty"`
	End        *Expression  `json:"end,omitempty"`
	Max        *Expression  `json:"max,omitempty"`
	Elements   []Expression `json:"elements,omitempty"`
	Entries    []KeyValue   `json:"entries,omitempty"`
	Items      []KeyValue   `json:"items,omitempty"`
	EmbedFiles []EmbedFile  `json:"embed_files,omitempty"`
	Func       FuncDecl     `json:"func,omitempty"`
}

type KeyValue struct {
	NodeID NodeID      `json:"node_id,omitempty"`
	Key    *Expression `json:"key,omitempty"`
	Value  Expression  `json:"value"`
}
