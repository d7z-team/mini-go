// Package optimize simplifies typed HIR without changing source evaluation order.
package optimize

import (
	"errors"
	"fmt"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
)

// Level selects the optional HIR pipeline. Level 0 emits lowered HIR without
// optional transformations; levels 1 and 2 enable progressively stronger,
// function-local passes.
type Level uint8

const (
	LevelNone Level = iota
	LevelDefault
	LevelFull
)

// Apply returns a deterministic, function-local optimization of program.
func Apply(program hir.Program, level Level) (hir.Program, error) {
	if level > LevelFull {
		return hir.Program{}, fmt.Errorf("unsupported HIR optimization level %d", level)
	}
	if level == LevelNone {
		return program, nil
	}
	out := program
	out.Functions = append([]hir.Function(nil), program.Functions...)
	logicalFunctions := make(map[string]hir.Function, len(program.Functions))
	for _, function := range program.Functions {
		if !function.RevisionLocal {
			logicalFunctions[function.ID] = function
		}
	}
	constants := make(map[string]*constant.Value, len(program.Constants))
	for _, constant := range program.Constants {
		constants[constant.ID] = constant.Value
	}
	for index := range out.Functions {
		body := cloneStatements(out.Functions[index].Body)
		optimized, err := optimizeBody(body, constants)
		if err != nil {
			return hir.Program{}, fmt.Errorf("function %s: %w", out.Functions[index].ID, err)
		}
		out.Functions[index].Body = optimized
		markDirectTailCall(&out.Functions[index], logicalFunctions)
		if level >= LevelFull {
			body, _ := foldPureBooleanBranches(out.Functions[index].Body)
			body, _ = propagateAdjacentBooleanLocals(body)
			body, err = optimizeBody(body, constants)
			if err != nil {
				return hir.Program{}, fmt.Errorf("function %s: %w", out.Functions[index].ID, err)
			}
			body, _ = removeDeadPureLocalStores(body, out.Functions[index].ResultLocals)
			out.Functions[index].Body = body
		}
	}
	return out, nil
}

func markDirectTailCall(function *hir.Function, logicalFunctions map[string]hir.Function) {
	if function == nil || len(function.ResultLocals) != 0 || len(function.Body) == 0 {
		return
	}
	last := &function.Body[len(function.Body)-1]
	if last.Kind != hir.StmtReturn || len(last.Results) != 1 {
		return
	}
	call := last.Results[0]
	if call.Kind != hir.ExprCallDirect || call.ResultCount != len(function.Signature.Results) {
		return
	}
	if call.ModulePath != "" {
		return
	}
	target, exists := logicalFunctions[call.Function]
	if !exists || len(target.Signature.Results) != len(function.Signature.Results) {
		return
	}
	for index := range target.Signature.Results {
		if target.Signature.Results[index] != function.Signature.Results[index] {
			return
		}
	}
	last.Kind = hir.StmtTailCallDirect
	last.Expr = call
	last.Results = nil
}

func cloneStatements(statements []hir.Statement) []hir.Statement {
	out := append([]hir.Statement(nil), statements...)
	for index := range out {
		out[index].SourcePoints = append([]hir.Location(nil), statements[index].SourcePoints...)
		out[index].TypeCases = append([]hir.TypeCase(nil), statements[index].TypeCases...)
	}
	return out
}

func optimizeBody(body []hir.Statement, constants map[string]*constant.Value) ([]hir.Statement, error) {
	limit := len(body)*2 + 8
	for iteration := 0; iteration < limit; iteration++ {
		if _, err := labelIndexes(body); err != nil {
			return nil, err
		}
		changed := false
		var passChanged bool
		body, passChanged = foldConstantBranches(body, constants)
		changed = changed || passChanged
		body, passChanged = coalesceLabels(body)
		changed = changed || passChanged
		body, passChanged = simplifyBranchFallthrough(body)
		changed = changed || passChanged
		var err error
		body, passChanged, err = threadJumps(body)
		if err != nil {
			return nil, err
		}
		changed = changed || passChanged
		body, passChanged, err = retainReachable(body)
		if err != nil {
			return nil, err
		}
		changed = changed || passChanged
		body, passChanged = removeRedundantJumps(body)
		changed = changed || passChanged
		body, passChanged = removeUnreferencedLabels(body)
		changed = changed || passChanged
		if !changed {
			return body, nil
		}
	}
	return nil, errors.New("control-flow optimization did not converge")
}

func foldConstantBranches(body []hir.Statement, constants map[string]*constant.Value) ([]hir.Statement, bool) {
	out := make([]hir.Statement, 0, len(body))
	var pending []hir.Location
	changed := false
	for _, statement := range body {
		if statement.Kind == hir.StmtJumpIf {
			if value, ok := constantBool(statement.Expr, constants); ok {
				changed = true
				if value != statement.BranchNegated {
					statement.Kind = hir.StmtJump
					statement.BranchNegated = false
					statement.Expr = hir.Expression{}
				} else {
					pending = appendUniqueLocations(pending, statement.SourcePoints)
					continue
				}
			}
		}
		if len(pending) != 0 {
			statement.SourcePoints = prependUniqueLocations(pending, statement.SourcePoints)
			pending = nil
		}
		out = append(out, statement)
	}
	if len(pending) != 0 && len(out) != 0 {
		out[len(out)-1].SourcePoints = appendUniqueLocations(out[len(out)-1].SourcePoints, pending)
	}
	return out, changed
}

// simplifyBranchFallthrough changes only the polarity of the conditional edge.
// Inverting a comparison operator would be incorrect for unordered floats.
func simplifyBranchFallthrough(body []hir.Statement) ([]hir.Statement, bool) {
	out := make([]hir.Statement, 0, len(body))
	changed := false
	for index := 0; index < len(body); index++ {
		statement := body[index]
		if statement.Kind == hir.StmtJumpIf && index+2 < len(body) && body[index+1].Kind == hir.StmtJump {
			for next := index + 2; next < len(body) && body[next].Kind == hir.StmtLabel; next++ {
				if body[next].Label == statement.Label {
					statement.Label = body[index+1].Label
					statement.BranchNegated = !statement.BranchNegated
					statement.SourcePoints = appendUniqueLocations(statement.SourcePoints, body[index+1].SourcePoints)
					index++
					changed = true
					break
				}
			}
		}
		out = append(out, statement)
	}
	return out, changed
}

func constantBool(expression hir.Expression, constants map[string]*constant.Value) (bool, bool) {
	var raw *constant.Value
	switch expression.Kind {
	case hir.ExprLiteral:
		raw = expression.Value
	case hir.ExprConst:
		raw = expression.Value
		if raw == nil {
			raw = constants[expression.ConstantID]
		}
	default:
		return false, false
	}
	if raw == nil || raw.Kind() != constant.Boolean {
		return false, false
	}
	return raw.Text == "true", true
}

func coalesceLabels(body []hir.Statement) ([]hir.Statement, bool) {
	aliases := make(map[string]string)
	out := make([]hir.Statement, 0, len(body))
	changed := false
	for _, statement := range body {
		if statement.Kind == hir.StmtLabel && len(out) != 0 && out[len(out)-1].Kind == hir.StmtLabel {
			canonical := out[len(out)-1].Label
			aliases[statement.Label] = canonical
			out[len(out)-1].SourcePoints = appendUniqueLocations(out[len(out)-1].SourcePoints, statement.SourcePoints)
			changed = true
			continue
		}
		out = append(out, statement)
	}
	if len(aliases) == 0 {
		return out, changed
	}
	for index := range out {
		if out[index].Kind != hir.StmtJump && out[index].Kind != hir.StmtJumpIf && out[index].Kind != hir.StmtTypeDispatch {
			continue
		}
		for {
			target, exists := aliases[out[index].Label]
			if !exists {
				break
			}
			out[index].Label = target
		}
		for i := range out[index].TypeCases {
			for {
				target, exists := aliases[out[index].TypeCases[i].Label]
				if !exists {
					break
				}
				out[index].TypeCases[i].Label = target
			}
		}
	}
	return out, true
}

func threadJumps(body []hir.Statement) ([]hir.Statement, bool, error) {
	labels, err := labelIndexes(body)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for index := range body {
		if body[index].Kind != hir.StmtJump && body[index].Kind != hir.StmtJumpIf {
			continue
		}
		target := body[index].Label
		seen := make(map[string]struct{})
		for {
			labelIndex, exists := labels[target]
			if !exists {
				return nil, false, fmt.Errorf("jump references unknown label %q", target)
			}
			if _, cycle := seen[target]; cycle {
				break
			}
			seen[target] = struct{}{}
			next := labelIndex + 1
			for next < len(body) && body[next].Kind == hir.StmtLabel {
				next++
			}
			if next >= len(body) || body[next].Kind != hir.StmtJump || body[next].Label == target {
				break
			}
			target = body[next].Label
		}
		if target != body[index].Label {
			body[index].Label = target
			changed = true
		}
	}
	return body, changed, nil
}

func retainReachable(body []hir.Statement) ([]hir.Statement, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	labels, err := labelIndexes(body)
	if err != nil {
		return nil, false, err
	}
	reachable := make([]bool, len(body))
	queue := []int{0}
	for len(queue) != 0 {
		index := queue[0]
		queue = queue[1:]
		if index < 0 || index >= len(body) || reachable[index] {
			continue
		}
		reachable[index] = true
		statement := body[index]
		switch statement.Kind {
		case hir.StmtTypeDispatch:
			target, exists := labels[statement.Label]
			if !exists {
				return nil, false, fmt.Errorf("type dispatch references unknown label %q", statement.Label)
			}
			queue = append(queue, target)
			for _, match := range statement.TypeCases {
				target, exists := labels[match.Label]
				if !exists {
					return nil, false, fmt.Errorf("type dispatch references unknown label %q", match.Label)
				}
				queue = append(queue, target)
			}
		case hir.StmtJump:
			target, exists := labels[statement.Label]
			if !exists {
				return nil, false, fmt.Errorf("jump references unknown label %q", statement.Label)
			}
			queue = append(queue, target)
		case hir.StmtJumpIf:
			target, exists := labels[statement.Label]
			if !exists {
				return nil, false, fmt.Errorf("jump references unknown label %q", statement.Label)
			}
			queue = append(queue, target, index+1)
		case hir.StmtReturn, hir.StmtTailCallDirect, hir.StmtPanic:
		default:
			queue = append(queue, index+1)
		}
	}
	out := make([]hir.Statement, 0, len(body))
	for index, statement := range body {
		if reachable[index] {
			out = append(out, statement)
		}
	}
	return out, len(out) != len(body), nil
}

func removeRedundantJumps(body []hir.Statement) ([]hir.Statement, bool) {
	remove := make([]bool, len(body))
	for index, statement := range body {
		if statement.Kind != hir.StmtJump {
			continue
		}
		for next := index + 1; next < len(body) && body[next].Kind == hir.StmtLabel; next++ {
			if body[next].Label == statement.Label {
				remove[index] = true
				break
			}
		}
	}
	return removeStatements(body, remove, true)
}

func removeUnreferencedLabels(body []hir.Statement) ([]hir.Statement, bool) {
	referenced := make(map[string]struct{})
	for _, statement := range body {
		if statement.Kind == hir.StmtJump || statement.Kind == hir.StmtJumpIf || statement.Kind == hir.StmtTypeDispatch {
			referenced[statement.Label] = struct{}{}
		}
		for _, match := range statement.TypeCases {
			referenced[match.Label] = struct{}{}
		}
	}
	remove := make([]bool, len(body))
	for index, statement := range body {
		if statement.Kind == hir.StmtLabel {
			_, keep := referenced[statement.Label]
			remove[index] = !keep
		}
	}
	return removeStatements(body, remove, true)
}

func removeStatements(body []hir.Statement, remove []bool, transferPoints bool) ([]hir.Statement, bool) {
	out := make([]hir.Statement, 0, len(body))
	var pending []hir.Location
	changed := false
	for index, statement := range body {
		if index < len(remove) && remove[index] {
			changed = true
			if transferPoints {
				pending = appendUniqueLocations(pending, statement.SourcePoints)
			}
			continue
		}
		if len(pending) != 0 {
			statement.SourcePoints = prependUniqueLocations(pending, statement.SourcePoints)
			pending = nil
		}
		out = append(out, statement)
	}
	if len(pending) != 0 && len(out) != 0 {
		out[len(out)-1].SourcePoints = appendUniqueLocations(out[len(out)-1].SourcePoints, pending)
	}
	return out, changed
}

func labelIndexes(body []hir.Statement) (map[string]int, error) {
	labels := make(map[string]int)
	for index, statement := range body {
		if statement.Kind != hir.StmtLabel {
			continue
		}
		if statement.Label == "" {
			return nil, fmt.Errorf("statement %d has an empty label", index)
		}
		if previous, exists := labels[statement.Label]; exists {
			return nil, fmt.Errorf("duplicate label %q at statements %d and %d", statement.Label, previous, index)
		}
		labels[statement.Label] = index
	}
	return labels, nil
}

func prependUniqueLocations(prefix, locations []hir.Location) []hir.Location {
	out := appendUniqueLocations(nil, prefix)
	return appendUniqueLocations(out, locations)
}

func appendUniqueLocations(locations, additions []hir.Location) []hir.Location {
	seen := make(map[hir.Location]struct{}, len(locations)+len(additions))
	out := append([]hir.Location(nil), locations...)
	for _, location := range out {
		seen[location] = struct{}{}
	}
	for _, location := range additions {
		if _, exists := seen[location]; exists {
			continue
		}
		out = append(out, location)
		seen[location] = struct{}{}
	}
	return out
}
