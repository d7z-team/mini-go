package lower

import (
	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func (l *lowerer) lowerEmbedInitializer(expr ast.Expression) (ir.Expression, bool) {
	info, ok := l.semantic.Embeds[expr.NodeID]
	if !ok || len(expr.EmbedFiles) == 0 {
		l.add("hirgen.embed.semantic", "embed initializer is missing semantic information", expr.Span)
		return ir.Expression{}, false
	}
	stringLiteral := func(value string, typ types.TypeRef) ir.Expression {
		return ir.Expression{Kind: ir.ExprLiteral, Type: typ, Value: stringConstant(value)}
	}
	switch info.Kind {
	case check.EmbedString:
		return stringLiteral(string(expr.EmbedFiles[0].Data), info.Type), true
	case check.EmbedBytes:
		return ir.Expression{
			Kind:  ir.ExprLiteral,
			Type:  info.Type,
			Value: stringConstant(string(expr.EmbedFiles[0].Data)),
		}, true
	case check.EmbedFS:
		keyType, valueType, ok := l.relations.View(info.Storage).Map()
		if !ok {
			l.add("hirgen.embed.storage", "embed.FS storage must be a map", expr.Span)
			return ir.Expression{}, false
		}
		entries := make([]ir.MapEntry, 0, len(expr.EmbedFiles))
		for _, file := range expr.EmbedFiles {
			data := stringLiteral(string(file.Data), types.Builtin(types.PrimitiveString))
			entries = append(entries, ir.MapEntry{
				Key:   stringLiteral(file.Path, keyType),
				Value: ir.Expression{Kind: ir.ExprConvert, Type: valueType, Operand: &data},
			})
		}
		storage := ir.Expression{Kind: ir.ExprMap, Type: info.Storage, Entries: entries}
		return ir.Expression{Kind: ir.ExprStruct, Type: info.Type, Fields: []ir.FieldValue{{Name: "files", Value: storage}}}, true
	default:
		l.add("hirgen.embed.kind", "embed initializer has an invalid semantic kind", expr.Span)
		return ir.Expression{}, false
	}
}
