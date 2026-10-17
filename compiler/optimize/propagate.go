package optimize

import (
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
)

// propagateLocalValues performs a forward, intersection data-flow pass over
// the function CFG. A fact survives a merge only when every predecessor agrees
// on the same immutable value or scalar copy. Writes kill the old fact before
// installing a fact for their right-hand side, and multi-assignment evaluates
// all right-hand sides against the incoming state first.
func propagateLocalValues(body []hir.Statement, table *types.TypeTable) []hir.Statement {
	cfg := buildControlFlow(body)
	protected := make(map[string]struct{})
	for _, statement := range body {
		collectStatementProtectedLocals(statement, protected)
	}
	// Facts are immutable scalar expressions. States share their payloads so
	// a CFG edge copies references rather than complete HIR expression trees.
	in := make([]map[string]*hir.Expression, len(body))
	out := make([]map[string]*hir.Expression, len(body))
	if len(body) != 0 {
		in[0] = map[string]*hir.Expression{}
	}
	for iteration, changed := 0, true; changed; iteration++ {
		if iteration >= len(body)*2+8 {
			return body
		}
		changed = false
		for index := range body {
			if index != 0 && len(cfg.predecessors[index]) == 0 {
				continue
			}
			incoming := in[index]
			if index != 0 {
				incoming = mergeFacts(cfg.predecessors[index], out)
			}
			if !equalFacts(in[index], incoming) {
				in[index] = incoming
				changed = true
			}
			transferred := transferFacts(body[index], incoming, protected, table)
			if !equalFacts(out[index], transferred) {
				out[index] = transferred
				changed = true
			}
		}
	}

	result := append([]hir.Statement(nil), body...)
	for index := range result {
		facts := in[index]
		if facts == nil {
			continue
		}
		statement := &result[index]
		if replacement, ok := replaceKnownLocals(statement.Expr, facts); ok {
			statement.Expr = replacement
		}
		if replacement, ok := replaceKnownLocals(statement.Object, facts); ok {
			statement.Object = replacement
		}
		if replacement, ok := replaceKnownLocals(statement.Index, facts); ok {
			statement.Index = replacement
		}
		for i := range statement.Results {
			if replacement, ok := replaceKnownLocals(statement.Results[i], facts); ok {
				statement.Results[i] = replacement
			}
		}
		for i := range statement.Values {
			if replacement, ok := replaceKnownLocals(statement.Values[i], facts); ok {
				statement.Values[i] = replacement
			}
		}
		for i := range statement.Args {
			if replacement, ok := replaceKnownLocals(statement.Args[i], facts); ok {
				statement.Args[i] = replacement
			}
		}
	}
	return result
}

func mergeFacts(predecessors []int, states []map[string]*hir.Expression) map[string]*hir.Expression {
	if len(predecessors) == 0 || states[predecessors[0]] == nil {
		return nil
	}
	merged := cloneFacts(states[predecessors[0]])
	for _, predecessor := range predecessors[1:] {
		state := states[predecessor]
		for local, fact := range merged {
			if state == nil || !equalFact(state[local], fact) {
				delete(merged, local)
			}
		}
	}
	return merged
}

func transferFacts(statement hir.Statement, incoming map[string]*hir.Expression, protected map[string]struct{}, table *types.TypeTable) map[string]*hir.Expression {
	if incoming == nil {
		return nil
	}
	state := cloneFacts(incoming)
	written := make(map[string]struct{})
	switch statement.Kind {
	case hir.StmtStoreLocal:
		fact, ok := knownValue(statement.Expr, state, table)
		written[statement.Local] = struct{}{}
		killWrittenFacts(state, written)
		if ok && safeCopyFact(fact, statement.Local, protected, written) {
			state[statement.Local] = fact
		}
	case hir.StmtStoreResults, hir.StmtStoreValues:
		facts := make([]*hir.Expression, len(statement.Targets))
		known := make([]bool, len(statement.Targets))
		if statement.Kind == hir.StmtStoreResults && len(statement.Targets) == 1 {
			facts[0], known[0] = knownValue(statement.Expr, state, table)
		} else if len(statement.Values) == len(statement.Targets) {
			for i := range statement.Targets {
				facts[i], known[i] = knownValue(statement.Values[i], state, table)
			}
		}
		for _, target := range statement.Targets {
			if target.Kind == "local" {
				written[target.Local] = struct{}{}
			}
		}
		killWrittenFacts(state, written)
		for i, target := range statement.Targets {
			if target.Kind != "local" {
				continue
			}
			// Repeated destinations are assigned from left to right. A later
			// unknown value must invalidate an earlier known value for that local.
			delete(state, target.Local)
			if known[i] && safeCopyFact(facts[i], target.Local, protected, written) {
				state[target.Local] = facts[i]
			}
		}
	case hir.StmtSelect, hir.StmtTypeDispatch:
		// These instructions write locals along a selected edge.
		clear(state)
	}
	return state
}

func killWrittenFacts(state map[string]*hir.Expression, written map[string]struct{}) {
	for local, fact := range state {
		_, targetWritten := written[local]
		_, sourceWritten := written[fact.Local]
		if targetWritten || fact.Kind == hir.ExprLocal && sourceWritten {
			delete(state, local)
		}
	}
}

func safeCopyFact(fact *hir.Expression, target string, protected, written map[string]struct{}) bool {
	if _, escaped := protected[target]; escaped {
		return false
	}
	if fact.Kind == hir.ExprLocal {
		if _, escaped := protected[fact.Local]; escaped {
			return false
		}
		if _, overwritten := written[fact.Local]; overwritten {
			return false
		}
	}
	return true
}

func knownValue(expression hir.Expression, state map[string]*hir.Expression, table *types.TypeTable) (*hir.Expression, bool) {
	switch expression.Kind {
	case hir.ExprLiteral, hir.ExprConst:
		return &expression, true
	case hir.ExprLocal:
		if fact, ok := state[expression.Local]; ok {
			return fact, true
		}
		if directScalarType(table, expression.Type) {
			return &expression, true
		}
	}
	return nil, false
}

func directScalarType(table *types.TypeTable, typ types.TypeRef) bool {
	if !typ.Valid() {
		return false
	}
	underlying := types.View(table, typ).Underlying()
	return underlying.Kind == types.Primitive && underlying.Primitive != types.PrimitiveComplex64 && underlying.Primitive != types.PrimitiveComplex128 && underlying.Primitive != types.PrimitiveFunction
}

func cloneFacts(input map[string]*hir.Expression) map[string]*hir.Expression {
	if input == nil {
		return nil
	}
	output := make(map[string]*hir.Expression, len(input))
	for local, fact := range input {
		output[local] = fact
	}
	return output
}

func equalFacts(left, right map[string]*hir.Expression) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for local, fact := range left {
		if other, ok := right[local]; !ok || !equalFact(other, fact) {
			return false
		}
	}
	return true
}

// The pass records only scalar literals, constants and copies. Compare their
// semantic payload directly rather than traversing unrelated expression trees.
func equalFact(left, right *hir.Expression) bool {
	if left == right {
		return true
	}
	if left == nil || right == nil || left.Kind != right.Kind || left.Type != right.Type || left.Untyped != right.Untyped {
		return false
	}
	switch left.Kind {
	case hir.ExprLocal:
		return left.Local == right.Local
	case hir.ExprConst:
		return left.ConstantID == right.ConstantID
	case hir.ExprLiteral:
		return left.Value == right.Value || left.Value != nil && right.Value != nil && *left.Value == *right.Value
	default:
		return false
	}
}

func replaceKnownLocals(expression hir.Expression, facts map[string]*hir.Expression) (hir.Expression, bool) {
	return replaceKnownLocalsScoped(expression, facts, nil)
}

// replaceKnownLocalsScoped keeps lexical let bindings out of the function-level
// fact map. HIR uses the same local identifier space for outer locals and let
// temporaries, so replacing through a binding would change which value a body
// expression reads.
func replaceKnownLocalsScoped(expression hir.Expression, facts map[string]*hir.Expression, shadowed map[string]struct{}) (hir.Expression, bool) {
	if expression.Kind == hir.ExprLocal {
		if _, blocked := shadowed[expression.Local]; blocked {
			return expression, false
		}
		if value, ok := facts[expression.Local]; ok {
			if value.Kind == hir.ExprLocal {
				if _, blocked := shadowed[value.Local]; blocked {
					return expression, false
				}
			}
			return *value, true
		}
		return expression, false
	}
	changed := false
	replace := func(child *hir.Expression) {
		if child == nil {
			return
		}
		if replacement, ok := replaceKnownLocalsScoped(*child, facts, shadowed); ok {
			*child = replacement
			changed = true
		}
	}
	if expression.Kind == hir.ExprLet || expression.Kind == hir.ExprLetResults {
		replace(expression.Bind)
		if expression.Body != nil {
			nested := make(map[string]struct{}, len(shadowed)+len(expression.Locals)+1)
			for local := range shadowed {
				nested[local] = struct{}{}
			}
			if expression.Kind == hir.ExprLet {
				nested[expression.Local] = struct{}{}
			} else {
				for _, local := range expression.Locals {
					nested[local] = struct{}{}
				}
			}
			if replacement, ok := replaceKnownLocalsScoped(*expression.Body, facts, nested); ok {
				*expression.Body = replacement
				changed = true
			}
		}
	} else {
		for _, child := range []*hir.Expression{expression.Left, expression.Right, expression.Operand, expression.Bind, expression.Body, expression.Size, expression.Index, expression.Start, expression.End, expression.Max} {
			replace(child)
		}
	}
	for i := range expression.Args {
		if replacement, ok := replaceKnownLocalsScoped(expression.Args[i], facts, shadowed); ok {
			expression.Args[i] = replacement
			changed = true
		}
	}
	for i := range expression.Elements {
		if replacement, ok := replaceKnownLocalsScoped(expression.Elements[i], facts, shadowed); ok {
			expression.Elements[i] = replacement
			changed = true
		}
	}
	for i := range expression.Entries {
		if replacement, ok := replaceKnownLocalsScoped(expression.Entries[i].Key, facts, shadowed); ok {
			expression.Entries[i].Key = replacement
			changed = true
		}
		if replacement, ok := replaceKnownLocalsScoped(expression.Entries[i].Value, facts, shadowed); ok {
			expression.Entries[i].Value = replacement
			changed = true
		}
	}
	for i := range expression.Fields {
		if replacement, ok := replaceKnownLocalsScoped(expression.Fields[i].Value, facts, shadowed); ok {
			expression.Fields[i].Value = replacement
			changed = true
		}
	}
	return expression, changed
}
