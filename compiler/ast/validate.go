package ast

import (
	"strings"

	"github.com/d7z-team/mini-go/compiler/source"
)

func ValidateProgram(program Program) []source.Diagnostic {
	return ValidateProgramWithLimits(program, Limits{})
}

// ValidateProgramWithLimits validates both the finite AST structure and the
// language-level tagged union rules.
func ValidateProgramWithLimits(program Program, limits Limits) []source.Diagnostic {
	limits = normalizeStructureLimits(limits)
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	collector.AddAll(ValidateStructure(&program, limits)...)
	if source.HasErrors(collector.Diagnostics()) {
		return collector.Diagnostics()
	}
	collector.AddAll(ValidateProgramContentsWithLimits(program, limits)...)
	return collector.Diagnostics()
}

// ValidateProgramContentsWithLimits validates language-level tagged union
// rules for an AST that has already passed ValidateStructure.
func ValidateProgramContentsWithLimits(program Program, limits Limits) []source.Diagnostic {
	limits = normalizeStructureLimits(limits)
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	add := func(code, message string, span source.Span) {
		collector.Add(source.Diagnostic{
			Code:     source.DiagnosticCode(code),
			Severity: "error",
			Message:  message,
			Primary:  span,
		})
	}
	if strings.TrimSpace(program.ModulePath) == "" {
		add("ast.module_path.missing", "missing module path", source.Span{})
	}
	packageName := strings.TrimSpace(program.Package)
	switch packageName {
	case "":
		add("ast.package.missing", "missing package name", source.Span{})
	case "_":
		add("ast.package.blank", "package name cannot be blank identifier", source.Span{})
	}
	seenFiles := map[string]struct{}{}
	seenDecls := map[string]struct{}{}
	for _, file := range program.Files {
		path := strings.TrimSpace(file.Path)
		if path == "" {
			add("ast.file.path.missing", "missing file path", file.Span)
		} else if _, ok := seenFiles[path]; ok {
			add("ast.file.path.duplicate", "duplicate file path", file.Span)
		} else {
			seenFiles[path] = struct{}{}
		}
		for _, decl := range file.Decls {
			validateDecl(decl, seenDecls, add)
		}
	}
	return collector.Diagnostics()
}

func validateDecl(decl Decl, seen map[string]struct{}, add func(string, string, source.Span)) {
	if !decl.hasPayload() {
		add("ast.decl.payload.missing", "missing declaration payload", decl.Span)
		return
	}
	switch decl.Kind {
	case DeclInvalid:
	case DeclImport:
		if strings.TrimSpace(decl.Import.Path) == "" {
			add("ast.import.path.missing", "missing import path", decl.Span)
		}
	case DeclConst:
		validateValueDecl("const", *decl.Const, add)
	case DeclVar:
		validateValueDecl("var", *decl.Var, add)
	case DeclType:
		name := strings.TrimSpace(decl.Type.Name)
		validateNamedDecl("type", name, decl.Span, seen, add)
		validateTypeParams(decl.Type.TypeParams, add)
		validateTypeExpr(decl.Type.Type, decl.Span, add)
	case DeclFunc:
		name := strings.TrimSpace(decl.Func.Name)
		if decl.Func.Receiver != nil && name != "_" {
			name = typeText(decl.Func.Receiver.Type) + "." + name
		}
		if decl.Func.Receiver == nil && name == "init" {
			validateFuncDecl(*decl.Func, decl.Span, add)
			return
		}
		validateNamedDecl("func", name, decl.Span, seen, add)
		validateFuncDecl(*decl.Func, decl.Span, add)
	default:
		add("ast.decl.kind.unknown", "unknown declaration kind", decl.Span)
	}
}

func validateValueDecl(kind string, decl ValueDecl, add func(string, string, source.Span)) {
	if len(decl.Names) == 0 {
		add("ast."+kind+".name.missing", "missing "+kind+" name", source.Span{})
	}
	for _, name := range decl.Names {
		if strings.TrimSpace(name) == "" {
			add("ast."+kind+".name.missing", "missing "+kind+" name", source.Span{})
		}
	}
	if decl.Type.Kind != TypeInvalid {
		validateTypeExpr(decl.Type, source.Span{}, add)
	}
	for _, expr := range decl.Values {
		validateExpression(expr, add)
	}
}

func validateNamedDecl(kind, name string, span source.Span, seen map[string]struct{}, add func(string, string, source.Span)) {
	if name == "" {
		add("ast."+kind+".name.missing", "missing "+kind+" name", span)
		return
	}
	if name == "_" {
		return
	}
	key := kind + ":" + name
	if _, ok := seen[key]; ok {
		add("ast."+kind+".name.duplicate", "duplicate "+kind+" name", span)
		return
	}
	seen[key] = struct{}{}
}

func typeText(typ TypeExpr) string {
	if strings.TrimSpace(typ.Name) != "" {
		return typ.Name
	}
	switch typ.Kind {
	case TypeInstance:
		if typ.Base == nil || len(typ.TypeArgs) == 0 {
			return ""
		}
		args := make([]string, len(typ.TypeArgs))
		for i := range typ.TypeArgs {
			args[i] = typeText(typ.TypeArgs[i])
		}
		return typeText(*typ.Base) + "[" + strings.Join(args, ",") + "]"
	case TypePointer:
		if typ.Elem == nil {
			return "Ptr<Any>"
		}
		return "Ptr<" + typeText(*typ.Elem) + ">"
	case TypeSlice:
		if typ.Elem == nil {
			return "Slice<Any>"
		}
		return "Slice<" + typeText(*typ.Elem) + ">"
	case TypeArray:
		length := arrayLengthText(typ)
		if length == "" {
			length = "?"
		}
		if typ.Elem == nil {
			return "Array<" + length + ", Any>"
		}
		return "Array<" + length + ", " + typeText(*typ.Elem) + ">"
	case TypeMap:
		if typ.Key == nil || typ.Elem == nil {
			return "Map<Any, Any>"
		}
		return "Map<" + typeText(*typ.Key) + ", " + typeText(*typ.Elem) + ">"
	case TypeInterface:
		if len(typ.Embeds) == 0 && len(typ.Terms) == 0 && len(typ.Methods) == 0 {
			return "interface{}"
		}
		members := make([]string, 0, len(typ.Embeds)+len(typ.Terms)+len(typ.Methods))
		for _, embed := range typ.Embeds {
			members = append(members, typeText(embed))
		}
		if len(typ.Terms) != 0 {
			terms := make([]string, 0, len(typ.Terms))
			for _, term := range typ.Terms {
				prefix := ""
				if term.Approx {
					prefix = "~"
				}
				terms = append(terms, prefix+typeText(term.Type))
			}
			members = append(members, strings.Join(terms, "|"))
		}
		for _, method := range typ.Methods {
			members = append(members, strings.TrimSpace(method.Name)+":function")
		}
		return "interface{" + strings.Join(members, ",") + "}"
	default:
		return string(typ.Kind)
	}
}

func arrayLengthText(typ TypeExpr) string {
	if typ.LenInfer {
		return "..."
	}
	if typ.Len == nil {
		return ""
	}
	if typ.Len.Kind == ExprLiteral {
		return strings.TrimSpace(typ.Len.Literal)
	}
	if typ.Len.Kind == ExprIdent {
		return strings.TrimSpace(typ.Len.Name)
	}
	return ""
}

func validateFuncDecl(decl FuncDecl, span source.Span, add func(string, string, source.Span)) {
	validateFuncSignature(decl, add)
	validateBlock(decl.Body, add)
	validateBranchTargets(decl.Body, add)
	if len(decl.Results) != 0 && !blockTerminates(decl.Body) {
		add("ast.func.return.missing", "function with results must end with a return or terminating statement", span)
	}
}

func validateFuncSignature(decl FuncDecl, add func(string, string, source.Span)) {
	validateTypeParams(decl.TypeParams, add)
	if decl.Receiver != nil {
		if decl.Receiver.Variadic {
			add("ast.func.receiver.variadic", "method receiver cannot be variadic", decl.Receiver.Span)
		}
		if !validReceiverTypeSyntax(decl.Receiver.Type) {
			add("ast.func.receiver.type", "method receiver type must be T or *T", decl.Receiver.Span)
		}
		validateTypeExpr(decl.Receiver.Type, decl.Receiver.Span, add)
	}
	for i, param := range decl.Params {
		if param.Variadic && i != len(decl.Params)-1 {
			add("ast.func.param.variadic.position", "variadic parameter must be last", param.Span)
		}
		validateTypeExpr(param.Type, param.Span, add)
	}
	for _, result := range decl.Results {
		if result.Variadic {
			add("ast.func.result.variadic", "function result cannot be variadic", result.Span)
		}
		validateTypeExpr(result.Type, result.Span, add)
	}
}

func validateTypeParams(params []TypeParam, add func(string, string, source.Span)) {
	seen := make(map[string]struct{}, len(params))
	for _, param := range params {
		name := strings.TrimSpace(param.Name)
		if name == "" {
			add("ast.type_param.name", "type parameter requires a name", param.Span)
		} else if name == "_" {
			// Blank type parameters occupy a position without declaring a name.
		} else if _, exists := seen[name]; exists {
			add("ast.type_param.duplicate", "duplicate type parameter name", param.Span)
		} else {
			seen[name] = struct{}{}
		}
		validateTypeExpr(param.Constraint, param.Span, add)
	}
}

func validReceiverTypeSyntax(typ TypeExpr) bool {
	switch typ.Kind {
	case TypeName:
		return strings.TrimSpace(typ.Name) != ""
	case TypeInstance:
		return typ.Base != nil && typ.Base.Kind == TypeName && strings.TrimSpace(typ.Base.Name) != ""
	case TypePointer:
		if typ.Elem == nil {
			return false
		}
		if typ.Elem.Kind == TypeName {
			return strings.TrimSpace(typ.Elem.Name) != ""
		}
		return typ.Elem.Kind == TypeInstance && typ.Elem.Base != nil && typ.Elem.Base.Kind == TypeName
	default:
		return false
	}
}
