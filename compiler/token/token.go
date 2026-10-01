// Package token defines Mini-Go lexical token kinds.
package token

type Kind string

const (
	Illegal Kind = "ILLEGAL"
	EOF     Kind = "EOF"
	Comment Kind = "COMMENT"

	Ident  Kind = "IDENT"
	Int    Kind = "INT"
	Float  Kind = "FLOAT"
	Imag   Kind = "IMAG"
	Char   Kind = "CHAR"
	String Kind = "STRING"

	Add Kind = "+"
	Sub Kind = "-"
	Mul Kind = "*"
	Quo Kind = "/"
	Rem Kind = "%"

	And    Kind = "&"
	Or     Kind = "|"
	Xor    Kind = "^"
	Tilde  Kind = "~"
	Shl    Kind = "<<"
	Shr    Kind = ">>"
	AndNot Kind = "&^"

	AddAssign    Kind = "+="
	SubAssign    Kind = "-="
	MulAssign    Kind = "*="
	QuoAssign    Kind = "/="
	RemAssign    Kind = "%="
	AndAssign    Kind = "&="
	OrAssign     Kind = "|="
	XorAssign    Kind = "^="
	ShlAssign    Kind = "<<="
	ShrAssign    Kind = ">>="
	AndNotAssign Kind = "&^="

	Land  Kind = "&&"
	Lor   Kind = "||"
	Arrow Kind = "<-"
	Inc   Kind = "++"
	Dec   Kind = "--"

	Eq     Kind = "=="
	Lt     Kind = "<"
	Gt     Kind = ">"
	Assign Kind = "="
	Not    Kind = "!"

	Ne       Kind = "!="
	Le       Kind = "<="
	Ge       Kind = ">="
	Define   Kind = ":="
	Ellipsis Kind = "..."

	Lparen    Kind = "("
	Lbrack    Kind = "["
	Lbrace    Kind = "{"
	Comma     Kind = ","
	Period    Kind = "."
	Semicolon Kind = ";"
	Colon     Kind = ":"
	Rparen    Kind = ")"
	Rbrack    Kind = "]"
	Rbrace    Kind = "}"

	Break       Kind = "BREAK"
	Case        Kind = "CASE"
	Chan        Kind = "CHAN"
	Const       Kind = "CONST"
	Continue    Kind = "CONTINUE"
	Default     Kind = "DEFAULT"
	Defer       Kind = "DEFER"
	Else        Kind = "ELSE"
	Fallthrough Kind = "FALLTHROUGH"
	For         Kind = "FOR"
	Func        Kind = "FUNC"
	Go          Kind = "GO"
	Goto        Kind = "GOTO"
	If          Kind = "IF"
	Import      Kind = "IMPORT"
	Interface   Kind = "INTERFACE"
	Map         Kind = "MAP"
	Package     Kind = "PACKAGE"
	Range       Kind = "RANGE"
	Return      Kind = "RETURN"
	Select      Kind = "SELECT"
	Struct      Kind = "STRUCT"
	Switch      Kind = "SWITCH"
	Type        Kind = "TYPE"
	Var         Kind = "VAR"
)

var keywords = map[string]Kind{
	"break":       Break,
	"case":        Case,
	"chan":        Chan,
	"const":       Const,
	"continue":    Continue,
	"default":     Default,
	"defer":       Defer,
	"else":        Else,
	"fallthrough": Fallthrough,
	"for":         For,
	"func":        Func,
	"go":          Go,
	"goto":        Goto,
	"if":          If,
	"import":      Import,
	"interface":   Interface,
	"map":         Map,
	"package":     Package,
	"range":       Range,
	"return":      Return,
	"select":      Select,
	"struct":      Struct,
	"switch":      Switch,
	"type":        Type,
	"var":         Var,
}

func Lookup(ident string) Kind {
	if kind, ok := keywords[ident]; ok {
		return kind
	}
	return Ident
}

func IsKeyword(kind Kind) bool {
	for _, keyword := range keywords {
		if keyword == kind {
			return true
		}
	}
	return false
}

func CanEndStatement(kind Kind) bool {
	switch kind {
	case Ident, Int, Float, Imag, Char, String,
		Break, Continue, Fallthrough, Return,
		Inc, Dec, Rparen, Rbrack, Rbrace:
		return true
	default:
		return false
	}
}

func IsIdentifierStart(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= 0x80 && inUnicodeRanges(letterRanges, r))
}

func IsIdentifierPart(r rune) bool {
	return IsIdentifierStart(r) || IsDecimalDigit(r) || (r >= 0x80 && inUnicodeRanges(digitRanges, r))
}

func IsDecimalDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func IsHexDigit(r rune) bool {
	return IsDecimalDigit(r) ||
		(r >= 'a' && r <= 'f') ||
		(r >= 'A' && r <= 'F')
}
