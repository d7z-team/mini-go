package optimize

import "github.com/d7z-team/mini-go/compiler/hir"

type inlineCandidate struct {
	parameters []string
	result     hir.Expression
}

// inlineSmallPureCalls substitutes bounded, single-result expression leaves.
// Arguments and the callee body must be side-effect free, every parameter is
// read at most once, and no non-parameter local may escape the callee frame.
// A compiled revision is immutable, so the resulting dependency is naturally
// discarded when a hot patch publishes the next revision.
func inlineSmallPureCalls(functions []hir.Function) []hir.Function {
	candidates := make(map[string]inlineCandidate)
	for _, function := range functions {
		if len(function.Upvalues) != 0 || len(function.ResultLocals) != 0 || function.Signature.Variadic ||
			len(function.Body) != 1 || function.Body[0].Kind != hir.StmtReturn || len(function.Body[0].Results) != 1 ||
			len(function.Signature.Results) != 1 || len(function.Signature.Params) > len(function.Locals) {
			continue
		}
		parameters := make([]string, len(function.Signature.Params))
		parameterSet := make(map[string]int, len(parameters))
		for index := range parameters {
			parameters[index] = function.Locals[index].ID
			parameterSet[parameters[index]] = index
		}
		uses := make([]int, len(parameters))
		nodes, safe := inspectInlineExpression(function.Body[0].Results[0], parameterSet, uses)
		if !safe || nodes > 16 {
			continue
		}
		valid := true
		for _, count := range uses {
			if count > 1 {
				valid = false
				break
			}
		}
		if valid {
			candidates[function.ID] = inlineCandidate{parameters: parameters, result: function.Body[0].Results[0]}
		}
	}
	if len(candidates) == 0 {
		return functions
	}
	out := append([]hir.Function(nil), functions...)
	for index := range out {
		for iteration := 0; iteration < 2; iteration++ {
			changed := false
			for statementIndex := range out[index].Body {
				statement := &out[index].Body[statementIndex]
				for _, expression := range statementExpressions(statement) {
					if rewriteInlineCalls(expression, out[index].ID, candidates) {
						changed = true
					}
				}
			}
			if !changed {
				break
			}
		}
	}
	return out
}

func inspectInlineExpression(expression hir.Expression, parameters map[string]int, uses []int) (int, bool) {
	switch expression.Kind {
	case hir.ExprLiteral, hir.ExprConst, hir.ExprZero:
	case hir.ExprLocal:
		index, ok := parameters[expression.Local]
		if !ok {
			return 0, false
		}
		uses[index]++
	case hir.ExprUnary:
	case hir.ExprBinary:
		if expression.Operator == "/" || expression.Operator == "%" || expression.Operator == "<<" || expression.Operator == ">>" {
			return 0, false
		}
	default:
		return 0, false
	}
	nodes := 1
	for _, child := range expressionChildren(&expression) {
		count, ok := inspectInlineExpression(*child, parameters, uses)
		if !ok {
			return 0, false
		}
		nodes += count
	}
	return nodes, true
}

func inlineArgumentSafe(expression hir.Expression) bool {
	switch expression.Kind {
	case hir.ExprLiteral, hir.ExprConst, hir.ExprZero, hir.ExprLocal, hir.ExprUpvalue, hir.ExprGlobal:
		return true
	case hir.ExprUnary, hir.ExprBinary:
		if expression.Operator == "/" || expression.Operator == "%" || expression.Operator == "<<" || expression.Operator == ">>" {
			return false
		}
		for _, child := range expressionChildren(&expression) {
			if !inlineArgumentSafe(*child) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func rewriteInlineCalls(expression *hir.Expression, owner string, candidates map[string]inlineCandidate) bool {
	if expression == nil {
		return false
	}
	changed := false
	for _, child := range expressionChildren(expression) {
		changed = rewriteInlineCalls(child, owner, candidates) || changed
	}
	for index := range expression.Args {
		changed = rewriteInlineCalls(&expression.Args[index], owner, candidates) || changed
	}
	for index := range expression.Elements {
		changed = rewriteInlineCalls(&expression.Elements[index], owner, candidates) || changed
	}
	for index := range expression.Entries {
		changed = rewriteInlineCalls(&expression.Entries[index].Key, owner, candidates) || changed
		changed = rewriteInlineCalls(&expression.Entries[index].Value, owner, candidates) || changed
	}
	for index := range expression.Fields {
		changed = rewriteInlineCalls(&expression.Fields[index].Value, owner, candidates) || changed
	}
	if expression.Kind != hir.ExprCallDirect || expression.ModulePath != "" || expression.Function == owner || expression.ResultCount != 1 {
		return changed
	}
	candidate, ok := candidates[expression.Function]
	if !ok || len(candidate.parameters) != len(expression.Args) {
		return changed
	}
	arguments := make(map[string]hir.Expression, len(candidate.parameters))
	for index, parameter := range candidate.parameters {
		if !inlineArgumentSafe(expression.Args[index]) {
			return changed
		}
		arguments[parameter] = cloneExpression(expression.Args[index])
	}
	replacement := substituteInlineParameters(cloneExpression(candidate.result), arguments)
	*expression = replacement
	return true
}

func substituteInlineParameters(expression hir.Expression, arguments map[string]hir.Expression) hir.Expression {
	if expression.Kind == hir.ExprLocal {
		if argument, ok := arguments[expression.Local]; ok {
			return cloneExpression(argument)
		}
	}
	for _, child := range expressionChildren(&expression) {
		*child = substituteInlineParameters(*child, arguments)
	}
	return expression
}

func expressionChildren(expression *hir.Expression) []*hir.Expression {
	all := []*hir.Expression{
		expression.Left, expression.Right, expression.Operand, expression.Bind, expression.Body,
		expression.Size, expression.Index, expression.Start, expression.End, expression.Max,
	}
	children := all[:0]
	for _, child := range all {
		if child != nil {
			children = append(children, child)
		}
	}
	return children
}

func statementExpressions(statement *hir.Statement) []*hir.Expression {
	expressions := []*hir.Expression{&statement.Expr, &statement.Object, &statement.Index}
	for index := range statement.Results {
		expressions = append(expressions, &statement.Results[index])
	}
	for index := range statement.Values {
		expressions = append(expressions, &statement.Values[index])
	}
	for index := range statement.Args {
		expressions = append(expressions, &statement.Args[index])
	}
	return expressions
}
