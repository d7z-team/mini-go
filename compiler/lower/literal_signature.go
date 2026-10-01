package lower

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

func literalValue(expr ast.Expression) (json.RawMessage, string, bool) {
	typ := ""
	if expr.Type != nil {
		typ = typeString(*expr.Type)
	}
	if typ == "" {
		typ = "Any"
	}
	text := strings.TrimSpace(expr.Literal)
	if text == "nil" {
		return json.RawMessage("null"), typ, true
	}
	switch typ {
	case "String":
		if len(text) == 0 || text[0] != '"' {
			text = strconv.Quote(text)
		}
		value, err := strconv.Unquote(text)
		if err != nil {
			return nil, "", false
		}
		return bytecode.EncodeStringConstant(value), typ, true
	case "Bool":
		if text != "true" && text != "false" {
			return nil, "", false
		}
	case "Int", "Int8", "Int16", "Int32", "Int64", "Uint", "Uint8", "Uint16", "Uint32", "Uint64", "Uintptr":
		if text == "" {
			return nil, "", false
		}
		value, ok := parseExactIntegerLiteral(text)
		if !ok {
			return nil, "", false
		}
		raw, ok := exactIntegerJSONRaw(value)
		return raw, typ, ok
	case "Float32", "Float64":
		value, ok := parseExactRationalLiteral(text)
		if !ok {
			return nil, "", false
		}
		raw, ok := exactRationalRaw(value)
		return raw, typ, ok
	case "Complex64", "Complex128":
		var value exactComplex
		var ok bool
		if strings.HasPrefix(text, "{") {
			value, ok = exactComplexFromRaw(json.RawMessage(text))
		} else {
			value, ok = parseExactImaginaryLiteral(text)
		}
		if !ok {
			return nil, "", false
		}
		raw, ok := exactComplexRaw(value)
		return raw, typ, ok
	default:
		if text == "" {
			return nil, "", false
		}
	}
	raw := json.RawMessage(text)
	if !json.Valid(raw) {
		return nil, "", false
	}
	return raw, typ, true
}

func isNilLiteral(expr ast.Expression) bool {
	return expr.Kind == ast.ExprLiteral && strings.TrimSpace(expr.Literal) == "nil"
}

func isEqualityOperator(op string) bool {
	switch strings.TrimSpace(op) {
	case "==", "!=":
		return true
	default:
		return false
	}
}

func isLogicalOperator(op string) bool {
	switch strings.TrimSpace(op) {
	case "&&", "||":
		return true
	default:
		return false
	}
}

func isOrderedComparisonOperator(op string) bool {
	switch strings.TrimSpace(op) {
	case "<", "<=", ">", ">=":
		return true
	default:
		return false
	}
}

func isShiftOperator(op string) bool {
	switch strings.TrimSpace(op) {
	case "<<", ">>":
		return true
	default:
		return false
	}
}

func signatureOf(fn ast.FuncDecl) string {
	params := make([]string, 0, len(fn.Params)+1)
	if fn.Receiver != nil {
		params = append(params, typeString(fn.Receiver.Type))
	}
	for _, param := range fn.Params {
		params = append(params, fieldTypeString(param))
	}
	results := make([]string, 0, len(fn.Results))
	for _, result := range fn.Results {
		results = append(results, typeString(result.Type))
	}
	resultText := "Void"
	if len(results) == 1 {
		resultText = results[0]
	} else if len(results) > 1 {
		resultText = "tuple(" + strings.Join(results, ", ") + ")"
	}
	return fmt.Sprintf("function(%s) %s", strings.Join(params, ", "), resultText)
}

func (l *lowerer) signatureOf(fn ast.FuncDecl) string {
	params := make([]string, 0, len(fn.Params)+1)
	if fn.Receiver != nil {
		params = append(params, l.resolveSourceType(fn.Receiver.Type))
	}
	for _, param := range fn.Params {
		params = append(params, l.fieldTypeString(param))
	}
	results := l.resultTypes(fn.Results)
	resultText := "Void"
	if len(results) == 1 {
		resultText = results[0]
	} else if len(results) > 1 {
		resultText = "tuple(" + strings.Join(results, ", ") + ")"
	}
	return fmt.Sprintf("function(%s) %s", strings.Join(params, ", "), resultText)
}

func signatureFromTypes(params, results []string) string {
	return functionValueSignature(params, results, false)
}

func functionValueSignature(params, results []string, variadic bool) string {
	params = append([]string(nil), params...)
	if variadic && len(params) > 0 {
		params[len(params)-1] = canonicalVariadicFunctionParam + params[len(params)-1]
	}
	resultText := "Void"
	if len(results) == 1 {
		resultText = results[0]
	} else if len(results) > 1 {
		resultText = "tuple(" + strings.Join(results, ", ") + ")"
	}
	return fmt.Sprintf("function(%s) %s", strings.Join(params, ", "), resultText)
}

func (l *lowerer) functionIdentitySignature(signature string, variadic bool) string {
	params, results, signatureVariadic, ok := l.functionSignatureParts(signature)
	if !ok {
		return strings.TrimSpace(signature)
	}
	return functionValueSignature(params, results, variadic || signatureVariadic)
}

func fieldTypeString(field ast.Field) string {
	typ := typeString(field.Type)
	if !field.Variadic {
		return typ
	}
	if typ == "" {
		return "Slice<Any>"
	}
	if field.Type.Kind == ast.TypeSlice {
		return typ
	}
	return "Slice<" + typ + ">"
}

func (l *lowerer) fieldTypeString(field ast.Field) string {
	typ := l.resolveSourceType(field.Type)
	if !field.Variadic {
		return typ
	}
	if typ == "" {
		return "Slice<Any>"
	}
	if l.isSliceType(typ) {
		return typ
	}
	return "Slice<" + typ + ">"
}

func (l *lowerer) sourceFunctionTypeString(params, results []ast.Field) string {
	parts := make([]string, 0, len(params))
	for i, param := range params {
		typ := l.fieldTypeString(param)
		if i == len(params)-1 && param.Variadic {
			typ = canonicalVariadicFunctionParam + typ
		}
		parts = append(parts, typ)
	}
	resultTypes := l.resultTypes(results)
	resultText := "Void"
	if len(resultTypes) == 1 {
		resultText = resultTypes[0]
	} else if len(resultTypes) > 1 {
		resultText = "tuple(" + strings.Join(resultTypes, ", ") + ")"
	}
	return fmt.Sprintf("function(%s) %s", strings.Join(parts, ", "), resultText)
}
