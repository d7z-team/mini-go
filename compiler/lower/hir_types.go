package lower

import (
	"fmt"
	"sort"

	ir "github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) hirSignature(text string, variadic bool) types.FunctionSignature {
	ref, ok := l.typeRef(text)
	if ok {
		if signature, function := l.typeTable.IsFunction(ref); function {
			signature.Variadic = signature.Variadic || variadic
			return signature
		}
	}
	l.add("hirgen.type.signature", fmt.Sprintf("invalid function signature %q", text), source.Span{})
	return types.FunctionSignature{}
}

func (l *lowerer) hirType(text string) types.TypeRef {
	if ref, ok := l.typeRef(text); ok {
		return ref
	}
	l.add("hirgen.type", fmt.Sprintf("invalid HIR type %q", text), source.Span{})
	return types.TypeRef{}
}

func (l *lowerer) finalizeHIRTypeTable(program *ir.Program) error {
	roots := make([]types.TypeRef, 0, len(program.Globals)+len(program.Functions))
	for i := range program.Constants {
		roots = append(roots, program.Constants[i].Type)
	}
	for i := range program.Globals {
		roots = append(roots, program.Globals[i].Type)
	}
	for i := range program.Functions {
		fn := &program.Functions[i]
		for _, param := range fn.Signature.Params {
			roots = append(roots, param.Type)
		}
		roots = append(roots, fn.Signature.Results...)
		for j := range fn.Locals {
			roots = append(roots, fn.Locals[j].Type)
		}
		for j := range fn.Upvalues {
			roots = append(roots, fn.Upvalues[j].Type)
		}
		for j := range fn.Body {
			collectStatementTypes(&fn.Body[j], &roots)
		}
	}
	for i := range program.Exports {
		roots = append(roots, program.Exports[i].Type)
	}
	artifactTable, err := l.typeTable.ReachableTable(roots...)
	if err != nil {
		return err
	}
	program.TypeTable.Nodes = append([]types.TypeNode(nil), artifactTable.Nodes...)
	sort.Slice(program.TypeTable.Nodes, func(i, j int) bool {
		return program.TypeTable.Nodes[i].ID < program.TypeTable.Nodes[j].ID
	})
	return program.TypeTable.Reindex()
}

func collectStatementTypes(stmt *ir.Statement, roots *[]types.TypeRef) {
	collectExpressionTypes(&stmt.Expr, roots)
	for i := range stmt.Results {
		collectExpressionTypes(&stmt.Results[i], roots)
	}
	for i := range stmt.Values {
		collectExpressionTypes(&stmt.Values[i], roots)
	}
	for i := range stmt.Args {
		collectExpressionTypes(&stmt.Args[i], roots)
	}
	collectExpressionTypes(&stmt.Object, roots)
	collectExpressionTypes(&stmt.Index, roots)
}

func collectExpressionTypes(expr *ir.Expression, roots *[]types.TypeRef) {
	if expr.Type.Valid() {
		*roots = append(*roots, expr.Type)
	}
	for _, child := range []*ir.Expression{expr.Left, expr.Right, expr.Operand, expr.Bind, expr.Body, expr.Size, expr.Index, expr.Start, expr.End, expr.Max} {
		if child != nil {
			collectExpressionTypes(child, roots)
		}
	}
	for i := range expr.Args {
		collectExpressionTypes(&expr.Args[i], roots)
	}
	for i := range expr.Elements {
		collectExpressionTypes(&expr.Elements[i], roots)
	}
	for i := range expr.Entries {
		collectExpressionTypes(&expr.Entries[i].Key, roots)
		collectExpressionTypes(&expr.Entries[i].Value, roots)
	}
	for i := range expr.Fields {
		collectExpressionTypes(&expr.Fields[i].Value, roots)
	}
}
