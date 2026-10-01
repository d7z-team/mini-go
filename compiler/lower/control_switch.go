package lower

import (
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
)

func (l *lowerer) lowerSwitchWithLabel(stmt ast.Statement, scope *funcScope, userLabel string) ([]ir.Statement, bool) {
	if stmt.TypeSwitch || strings.TrimSpace(stmt.TypeSwitchName) != "" {
		return l.lowerTypeSwitchWithLabel(stmt, scope, userLabel)
	}
	switchScope := l.childScope(scope)
	endLabel := l.newLabel("switch.end")
	defaultLabel := endLabel
	caseLabels := make([]string, len(stmt.Cases))
	for i, clause := range stmt.Cases {
		caseLabels[i] = l.newLabel("switch.case")
		if clause.Default {
			defaultLabel = caseLabels[i]
		}
	}
	var out []ir.Statement
	if stmt.Init != nil {
		init, ok := l.lowerStatement(*stmt.Init, switchScope)
		if !ok {
			return nil, false
		}
		out = append(out, init...)
	}
	var tagRef *ir.Expression
	tagType := ""
	switchFact, hasSwitchFact := l.semanticSwitchFact(stmt)
	if stmt.Expr != nil {
		tagExpr, ok := l.lowerExpression(*stmt.Expr, switchScope)
		if !ok {
			return nil, false
		}
		if hirResultCount(tagExpr) != 1 {
			l.add("hirgen.switch.tag.result", "switch tag expression must produce exactly one result", stmt.Expr.Span)
			return nil, false
		}
		if hasSwitchFact && switchFact.Tag.Valid() {
			tagType = l.formatSemanticType(switchFact.Tag)
		} else {
			tagType = l.expressionType(*stmt.Expr, switchScope)
		}
		tagLocal := l.newSyntheticLocal(switchScope, "switch.tag", tagType)
		out = append(out, ir.Statement{Kind: ir.StmtStoreLocal, Local: tagLocal, Expr: tagExpr})
		tagRefExpr := ir.Expression{Kind: ir.ExprLocal, Local: tagLocal}
		tagRef = &tagRefExpr
	}
	if tagRef != nil {
		if strings.TrimSpace(tagType) == "" {
			l.add("hirgen.switch.tag.type", "switch tag expression has no type", stmt.Expr.Span)
			return nil, false
		}
		comparable := l.isComparableType(tagType)
		if hasSwitchFact && switchFact.Tag.Valid() {
			comparable = switchFact.Comparable
		}
		if !comparable {
			l.add("hirgen.switch.tag.comparable", "switch tag expression must be comparable", stmt.Expr.Span)
			return nil, false
		}
	}
	if !l.validateSwitchCases(stmt.Cases, tagType, switchScope) {
		return nil, false
	}
	for i, clause := range stmt.Cases {
		if clause.Default {
			continue
		}
		for _, value := range clause.Values {
			cond, ok := l.lowerSwitchCaseCondition(tagRef, tagType, value, switchScope)
			if !ok {
				return nil, false
			}
			out = append(out, ir.Statement{Kind: ir.StmtJumpIf, Expr: cond, Label: caseLabels[i]})
		}
	}
	out = append(out, ir.Statement{Kind: ir.StmtJump, Label: defaultLabel})
	for i, clause := range stmt.Cases {
		afterCase := l.newLabel("switch.after_case")
		out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: caseLabels[i]})
		fallthroughLabel := ""
		if i+1 < len(caseLabels) {
			fallthroughLabel = caseLabels[i+1]
		}
		if !l.validateSwitchFallthrough(clause, i, len(stmt.Cases), false) {
			return nil, false
		}
		l.pushBranchWithUserLabel(userLabel, endLabel, "", fallthroughLabel)
		body, ok := l.lowerBlock(clause.Body, switchScope)
		l.popBranch()
		if !ok {
			return nil, false
		}
		out = append(out, body...)
		out = append(out,
			ir.Statement{Kind: ir.StmtLabel, Label: afterCase},
			ir.Statement{Kind: ir.StmtJump, Label: endLabel},
		)
	}
	out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: endLabel})
	return out, true
}

func (l *lowerer) validateSwitchFallthrough(clause ast.CaseClause, index, count int, typeSwitch bool) bool {
	if !blockContainsFallthrough(clause.Body) {
		return true
	}
	if typeSwitch {
		l.add("hirgen.branch.fallthrough.typeswitch", "fallthrough is not allowed in type switch", clause.Span)
		return false
	}
	if index == count-1 {
		l.add("hirgen.branch.fallthrough.final", "fallthrough cannot appear in the final switch case", clause.Span)
		return false
	}
	if !blockHasOnlyFinalFallthrough(clause.Body) {
		l.add("hirgen.branch.fallthrough.position", "fallthrough must be the final statement in a switch case", clause.Span)
		return false
	}
	return true
}

func blockHasOnlyFinalFallthrough(block ast.BlockStmt) bool {
	if len(block.Stmts) == 0 {
		return false
	}
	last := block.Stmts[len(block.Stmts)-1]
	if last.Kind != ast.StmtBranch || strings.TrimSpace(last.Op) != "fallthrough" {
		return false
	}
	for _, stmt := range block.Stmts[:len(block.Stmts)-1] {
		if statementContainsFallthrough(stmt) {
			return false
		}
	}
	return true
}

func blockContainsFallthrough(block ast.BlockStmt) bool {
	for _, stmt := range block.Stmts {
		if statementContainsFallthrough(stmt) {
			return true
		}
	}
	return false
}

func statementContainsFallthrough(stmt ast.Statement) bool {
	if stmt.Kind == ast.StmtBranch && strings.TrimSpace(stmt.Op) == "fallthrough" {
		return true
	}
	if stmt.Kind == ast.StmtSwitch || stmt.Kind == ast.StmtSelect {
		return false
	}
	if blockContainsFallthrough(stmt.Body) {
		return true
	}
	if stmt.Else != nil && statementContainsFallthrough(*stmt.Else) {
		return true
	}
	return false
}

func (l *lowerer) lowerSwitchCaseCondition(tag *ir.Expression, tagType string, value ast.Expression, scope *funcScope) (ir.Expression, bool) {
	var right ir.Expression
	var ok bool
	if tag != nil && strings.TrimSpace(tagType) != "" {
		right, ok = l.lowerExpressionInType(value, tagType, scope)
	} else {
		right, ok = l.lowerExpression(value, scope)
	}
	if !ok {
		return ir.Expression{}, false
	}
	if tag == nil {
		return l.lowerBoolOperand(value, scope)
	}
	return ir.Expression{
		Kind:     ir.ExprBinary,
		Operator: "==",
		Left:     tag,
		Right:    &right,
	}, true
}

func (l *lowerer) validateSwitchCases(cases []ast.CaseClause, tagType string, scope *funcScope) bool {
	seenDefault := false
	seenConstants := map[string]struct{}{}
	valid := true
	for _, clause := range cases {
		if clause.Default {
			if seenDefault {
				l.add("hirgen.switch.default.duplicate", "switch can contain only one default case", clause.Span)
				valid = false
			}
			seenDefault = true
			continue
		}
		if len(clause.Values) == 0 {
			l.add("hirgen.switch.case.empty", "switch case requires at least one expression", clause.Span)
			valid = false
			continue
		}
		for _, value := range clause.Values {
			if tagType == "" {
				valueType := l.expressionType(value, scope)
				if valueType == "" || l.resolveNamedUnderlyingType(valueType) != "Bool" {
					l.add("hirgen.switch.case.bool", "expressionless switch case must be bool", value.Span)
					valid = false
				}
			}
			key, ok := l.switchCaseConstantKey(value, tagType, scope)
			if !ok {
				continue
			}
			if _, exists := seenConstants[key]; exists {
				l.add("hirgen.switch.case.duplicate", "switch contains duplicate constant case", value.Span)
				valid = false
			} else {
				seenConstants[key] = struct{}{}
			}
		}
	}
	return valid
}

func (l *lowerer) switchCaseConstantKey(value ast.Expression, tagType string, scope *funcScope) (string, bool) {
	if isNilLiteral(value) {
		return "nil", true
	}
	raw, sourceType, ok := l.constValue(value, scope)
	if !ok || strings.TrimSpace(sourceType) == "" {
		return "", false
	}
	keyType := l.resolveType(sourceType)
	if tagType != "" && tagType != "Any" {
		converted, convertedType, convertedOK := l.convertConstValue(raw, keyType, tagType)
		if convertedOK {
			raw = converted
			keyType = convertedType
		}
	}
	keyType = l.typeIdentity(keyType)
	if keyType == "" {
		keyType = l.resolveType(sourceType)
	}
	return keyType + ":" + string(raw.JSON()), true
}

func (l *lowerer) lowerTypeSwitchWithLabel(stmt ast.Statement, scope *funcScope, userLabel string) ([]ir.Statement, bool) {
	if stmt.Expr == nil {
		l.add("hirgen.typeswitch.expr.missing", "type switch requires a subject expression", stmt.Span)
		return nil, false
	}
	switchFact, hasSwitchFact := l.semanticSwitchFact(stmt)
	subjectType := l.expressionType(*stmt.Expr, scope)
	if hasSwitchFact && switchFact.Subject.Valid() {
		subjectType = l.formatSemanticType(switchFact.Subject)
	}
	if strings.TrimSpace(subjectType) == "" {
		l.add("hirgen.typeswitch.subject.type", "type switch subject has no type", stmt.Expr.Span)
		return nil, false
	}
	interfaceSubject := l.isInterfaceValueType(subjectType)
	if hasSwitchFact {
		interfaceSubject = switchFact.Interface
	}
	if !interfaceSubject {
		l.add("hirgen.typeswitch.subject.interface", "type switch subject must have interface type", stmt.Expr.Span)
		return nil, false
	}
	typeSetSubject := l.isGeneralInterfaceType(subjectType)
	if hasSwitchFact {
		typeSetSubject = switchFact.TypeSet
	}
	if typeSetSubject {
		l.add("hirgen.typeswitch.subject.constraint", "type switch subject cannot use a type-set interface", stmt.Expr.Span)
		return nil, false
	}
	if !l.validateTypeSwitchCases(subjectType, stmt.Cases, switchFact, hasSwitchFact) {
		return nil, false
	}
	switchScope := l.childScope(scope)
	endLabel := l.newLabel("typeswitch.end")
	defaultLabel := endLabel
	caseLabels := make([]string, len(stmt.Cases))
	caseScopes := make([]*funcScope, len(stmt.Cases))
	caseVars := make([]string, len(stmt.Cases))
	for i, clause := range stmt.Cases {
		caseLabels[i] = l.newLabel("typeswitch.case")
		caseScopes[i] = l.childScope(switchScope)
		if clause.Default {
			defaultLabel = caseLabels[i]
		}
		if name := strings.TrimSpace(stmt.TypeSwitchName); name != "" {
			varType := ""
			if hasSwitchFact && i < len(switchFact.Cases) {
				varType = l.formatSemanticType(switchFact.Cases[i].Binding)
			}
			if varType == "" {
				varType = subjectType
			}
			local := l.newSyntheticLocal(caseScopes[i], "typeswitch."+name, varType)
			caseScopes[i].locals[name] = local
			caseScopes[i].localTypes[name] = l.hirType(varType)
			caseVars[i] = local
		}
	}
	var out []ir.Statement
	if stmt.Init != nil {
		init, ok := l.lowerStatement(*stmt.Init, switchScope)
		if !ok {
			return nil, false
		}
		out = append(out, init...)
	}
	subject, ok := l.lowerExpression(*stmt.Expr, switchScope)
	if !ok {
		return nil, false
	}
	if hirResultCount(subject) != 1 {
		l.add("hirgen.typeswitch.subject.result", "type switch subject must produce exactly one result", stmt.Expr.Span)
		return nil, false
	}
	if !hasSwitchFact || !switchFact.Subject.Valid() {
		subjectType = l.expressionType(*stmt.Expr, switchScope)
	}
	subjectLocal := l.newSyntheticLocal(switchScope, "typeswitch.subject", subjectType)
	out = append(out, ir.Statement{Kind: ir.StmtStoreLocal, Local: subjectLocal, Expr: subject})
	dispatch := ir.Statement{Kind: ir.StmtTypeDispatch, Local: subjectLocal, Label: defaultLabel}
	for i, clause := range stmt.Cases {
		if clause.Default {
			dispatch.DefaultLocal = caseVars[i]
			continue
		}
		if clause.Nil {
			dispatch.TypeCases = append(dispatch.TypeCases, ir.TypeCase{Label: caseLabels[i], Binding: caseVars[i], Original: true})
		}
		for typeIndex, typ := range clause.Types {
			targetType := ""
			if hasSwitchFact && i < len(switchFact.Cases) && typeIndex < len(switchFact.Cases[i].Types) {
				targetType = l.formatSemanticType(switchFact.Cases[i].Types[typeIndex])
			}
			if targetType == "" {
				targetType = l.resolveSourceType(typ)
			}
			if strings.TrimSpace(targetType) == "" {
				l.add("hirgen.typeswitch.case.type", "type switch case requires a concrete type", clause.Span)
				return nil, false
			}
			dispatch.TypeCases = append(dispatch.TypeCases, ir.TypeCase{
				Type: l.hirType(targetType), Label: caseLabels[i], Binding: caseVars[i],
				Original: clause.Nil || len(clause.Types) != 1,
			})
		}
	}
	out = append(out, dispatch)
	for i, clause := range stmt.Cases {
		out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: caseLabels[i]})
		fallthroughLabel := ""
		if i+1 < len(caseLabels) {
			fallthroughLabel = caseLabels[i+1]
		}
		if !l.validateSwitchFallthrough(clause, i, len(stmt.Cases), true) {
			return nil, false
		}
		l.pushBranchWithUserLabel(userLabel, endLabel, "", fallthroughLabel)
		body, ok := l.lowerBlock(clause.Body, caseScopes[i])
		l.popBranch()
		if !ok {
			return nil, false
		}
		out = append(out, body...)
		out = append(out, ir.Statement{Kind: ir.StmtJump, Label: endLabel})
	}
	out = append(out, ir.Statement{Kind: ir.StmtLabel, Label: endLabel})
	return out, true
}

func (l *lowerer) validateTypeSwitchCases(subjectType string, cases []ast.CaseClause, switchFact check.SwitchInfo, hasSwitchFact bool) bool {
	subjectType = l.resolveType(strings.TrimSpace(subjectType))
	seenTypes := map[string]struct{}{}
	seenNil := false
	seenDefault := false
	valid := true
	for caseIndex, clause := range cases {
		if clause.Default {
			if seenDefault {
				l.add("hirgen.typeswitch.default.duplicate", "type switch can contain only one default case", clause.Span)
				valid = false
			}
			seenDefault = true
			continue
		}
		if clause.Nil {
			if seenNil {
				l.add("hirgen.typeswitch.nil.duplicate", "type switch cannot contain duplicate nil cases", clause.Span)
				valid = false
			}
			seenNil = true
		}
		if !clause.Nil && len(clause.Types) == 0 {
			l.add("hirgen.typeswitch.case.empty", "type switch case requires nil or a type", clause.Span)
			valid = false
		}
		for typeIndex, typ := range clause.Types {
			targetType := ""
			if hasSwitchFact && caseIndex < len(switchFact.Cases) && typeIndex < len(switchFact.Cases[caseIndex].Types) {
				targetType = l.formatSemanticType(switchFact.Cases[caseIndex].Types[typeIndex])
			}
			if targetType == "" {
				targetType = l.resolveSourceType(typ)
			}
			if strings.TrimSpace(targetType) == "" {
				l.add("hirgen.typeswitch.case.type", "type switch case requires a type", clause.Span)
				valid = false
				continue
			}
			typeSet := l.isGeneralInterfaceType(targetType)
			if hasSwitchFact && caseIndex < len(switchFact.Cases) && typeIndex < len(switchFact.Cases[caseIndex].TypeSets) {
				typeSet = switchFact.Cases[caseIndex].TypeSets[typeIndex]
			}
			if typeSet {
				l.add("hirgen.typeswitch.case.constraint", "type switch case cannot use a type-set interface", typ.Span)
				valid = false
				continue
			}
			key := l.typeIdentity(targetType)
			if key == "" {
				key = targetType
			}
			if _, exists := seenTypes[key]; exists {
				l.add("hirgen.typeswitch.case.duplicate", "type switch contains duplicate case type", typ.Span)
				valid = false
			} else {
				seenTypes[key] = struct{}{}
			}
			implements := l.typeSwitchCaseImplements(subjectType, targetType)
			if hasSwitchFact && caseIndex < len(switchFact.Cases) && typeIndex < len(switchFact.Cases[caseIndex].Implements) {
				implements = switchFact.Cases[caseIndex].Implements[typeIndex]
			}
			if !implements {
				l.add("hirgen.typeswitch.case.implements", fmt.Sprintf("type switch case %s does not implement subject interface %s", targetType, subjectType), typ.Span)
				valid = false
			}
		}
	}
	return valid
}

func (l *lowerer) typeSwitchCaseImplements(subjectType, targetType string) bool {
	if subjectType == "Any" || len(l.interfaceMethods(subjectType, map[string]struct{}{})) == 0 {
		return true
	}
	return l.implementsInterfaceType(targetType, subjectType)
}
