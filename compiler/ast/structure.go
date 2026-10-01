// Package ast defines the parsed Mini-Go syntax tree and its structural validation.
package ast

import "github.com/d7z-team/mini-go/compiler/source"

const (
	DefaultMaxDepth       = 512
	DefaultMaxNodes       = 1_000_000
	DefaultMaxDiagnostics = 100
)

// Limits bounds validation of an AST supplied by the parser or a serialized
// compiler boundary. Zero values select the compiler defaults.
type Limits struct {
	MaxDepth       int
	MaxNodes       int
	MaxDiagnostics int
}

// StructureStats reports facts collected while validating an AST structure.
// Nodes includes shared subtree occurrences, bounding recursive consumers.
type StructureStats struct {
	Nodes int
	// FileNodes counts each file subtree without the package root.
	FileNodes []int
}

type structureNode struct {
	value                            any
	depth                            int
	exit                             bool
	track                            bool
	nodesBefore, parentExit, deepest int
}

type structureMemo struct {
	state         uint8
	nodes, height int
}

// ValidateStructure verifies the finite tree and source-location invariants
// required by all recursive AST consumers. Invalid recovery nodes are allowed.
func ValidateStructure(program *Program, limits Limits) []source.Diagnostic {
	diagnostics, _ := ValidateStructureWithStats(program, limits)
	return diagnostics
}

// ValidateStructureWithStats validates an AST structure and returns statistics
// gathered by the same traversal.
func ValidateStructureWithStats(program *Program, limits Limits) ([]source.Diagnostic, StructureStats) {
	return walkStructure(program, limits, false)
}

// FinalizeStructure verifies a bounded tree and assigns deterministic node and
// name identities during the same traversal. The caller owns the mutable tree.
// A failing tree is not suitable for semantic analysis.
func FinalizeStructure(program *Program, limits Limits) ([]source.Diagnostic, StructureStats) {
	return walkStructure(program, limits, true)
}

func walkStructure(program *Program, limits Limits, assignIDs bool) ([]source.Diagnostic, StructureStats) {
	limits = normalizeStructureLimits(limits)
	collector := source.NewDiagnosticCollector(limits.MaxDiagnostics)
	if program == nil {
		collector.Add(structureDiagnostic("ast.program.missing", "missing program", source.Span{}))
		return collector.Diagnostics(), StructureStats{}
	}

	fileEnds := make(map[string]int, len(program.Files))
	for i := range program.Files {
		file := &program.Files[i]
		if file.Path != "" {
			fileEnds[file.Path] = -1
			if file.Span.Valid() && file.Span.Start.File == file.Path {
				fileEnds[file.Path] = file.Span.End.Offset
			}
		}
	}
	seenPointers := map[any]structureMemo{}
	seenNodeIDs := map[NodeID]source.Span{}
	nodes := 0
	var fileStarts []int
	nextNode := NodeID(1)
	nextName := NameID(1)
	stopped := false
	stack := []structureNode{{value: program, depth: 1}}
	activeExit := -1

	addNode := func(id *NodeID, span source.Span, depth int) bool {
		if depth > limits.MaxDepth {
			collector.Add(structureDiagnostic("ast.limit.depth", "AST nesting depth exceeds compiler limit", span))
			stopped = true
			return false
		}
		nodes++
		if nodes > limits.MaxNodes {
			collector.Add(structureDiagnostic("ast.limit.nodes", "AST node count exceeds compiler limit", span))
			stopped = true
			return false
		}
		if assignIDs {
			*id, nextNode = nextNode, nextNode+1
		} else if *id != 0 {
			if previous, exists := seenNodeIDs[*id]; exists && previous != span {
				collector.Add(structureDiagnostic("ast.node_id.duplicate", "duplicate AST node ID", span))
			} else {
				seenNodeIDs[*id] = span
			}
		}
		if activeExit >= 0 {
			stack[activeExit].deepest = max(stack[activeExit].deepest, depth)
		}
		validateStructureSpan(span, fileEnds, collector)
		return true
	}
	addIdentifier := func(identifier *Identifier) {
		if assignIDs && identifier.Text != "" {
			identifier.ID, nextName = nextName, nextName+1
		}
		validateStructureSpan(identifier.Span, fileEnds, collector)
	}
	push := func(value any, depth int) {
		if value == nil || stopped {
			return
		}
		if len(stack) >= limits.MaxNodes+limits.MaxDepth {
			collector.Add(structureDiagnostic("ast.limit.nodes", "AST node count exceeds compiler limit", source.Span{}))
			stopped = true
			return
		}
		stack = append(stack, structureNode{value: value, depth: depth})
	}
	pushTracked := func(value any, depth int) {
		if stopped {
			return
		}
		if len(stack) >= limits.MaxNodes+limits.MaxDepth {
			collector.Add(structureDiagnostic("ast.limit.nodes", "AST node count exceeds compiler limit", source.Span{}))
			stopped = true
			return
		}
		stack = append(stack, structureNode{value: value, depth: depth, track: true})
	}

	for len(stack) != 0 && !stopped {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if item.exit {
			seenPointers[item.value] = structureMemo{state: 2, nodes: nodes - item.nodesBefore, height: max(0, item.deepest-item.depth+1)}
			activeExit = item.parentExit
			if activeExit >= 0 {
				stack[activeExit].deepest = max(stack[activeExit].deepest, item.deepest)
			}
			continue
		}
		if item.track {
			memo := seenPointers[item.value]
			switch memo.state {
			case 1:
				collector.Add(structureDiagnostic("ast.pointer.cycle", "AST contains a pointer cycle", source.Span{}))
				stopped = true
				continue
			case 2:
				deepest := item.depth + memo.height - 1
				if memo.nodes != 0 && deepest > limits.MaxDepth {
					collector.Add(structureDiagnostic("ast.limit.depth", "AST nesting depth exceeds compiler limit", source.Span{}))
					stopped = true
					continue
				}
				if memo.nodes > limits.MaxNodes-nodes {
					nodes = limits.MaxNodes + 1
					collector.Add(structureDiagnostic("ast.limit.nodes", "AST node count exceeds compiler limit", source.Span{}))
					stopped = true
					continue
				}
				nodes += memo.nodes
				if activeExit >= 0 && memo.nodes != 0 {
					stack[activeExit].deepest = max(stack[activeExit].deepest, deepest)
				}
				continue
			}
			seenPointers[item.value] = structureMemo{state: 1}
			parentExit := activeExit
			activeExit = len(stack)
			stack = append(stack, structureNode{value: item.value, depth: item.depth, exit: true, nodesBefore: nodes, parentExit: parentExit})
		}
		next := item.depth + 1
		switch node := item.value.(type) {
		case *Program:
			if !addNode(&node.NodeID, source.Span{}, item.depth) {
				continue
			}
			addIdentifier(&node.PackageID)
			for i := len(node.Files) - 1; i >= 0 && !stopped; i-- {
				push(&node.Files[i], next)
			}
		case *File:
			fileStarts = append(fileStarts, nodes)
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			addIdentifier(&node.PackageID)
			for i := len(node.Decls) - 1; i >= 0 && !stopped; i-- {
				push(&node.Decls[i], next)
			}
		case *Decl:
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			if !node.hasPayload() {
				collector.Add(structureDiagnostic("ast.decl.payload.missing", "missing declaration payload", node.Span))
				continue
			}
			switch node.Kind {
			case DeclImport:
				validateStructureSpan(node.Import.PathSpan, fileEnds, collector)
				addIdentifier(&node.Import.AliasID)
			case DeclConst:
				pushTracked(node.Const, next)
			case DeclVar:
				pushTracked(node.Var, next)
			case DeclType:
				pushTracked(node.Type, next)
			case DeclFunc:
				pushTracked(node.Func, next)
			case DeclInvalid:
			default:
				collector.Add(structureDiagnostic("ast.decl.kind.unknown", "unknown declaration kind", node.Span))
			}
		case *ValueDecl:
			for i := range node.NameIDs {
				addIdentifier(&node.NameIDs[i])
			}
			for i := len(node.Values) - 1; i >= 0 && !stopped; i-- {
				push(&node.Values[i], next)
			}
			if node.Type.Kind != TypeInvalid {
				push(&node.Type, next)
			}
		case *TypeDecl:
			addIdentifier(&node.NameID)
			push(&node.Type, next)
			for i := len(node.TypeParams) - 1; i >= 0 && !stopped; i-- {
				push(&node.TypeParams[i], next)
			}
		case *FuncDecl:
			if !addNode(&node.NodeID, node.Body.Span, item.depth) {
				continue
			}
			addIdentifier(&node.NameID)
			push(&node.Body, next)
			for i := len(node.Results) - 1; i >= 0 && !stopped; i-- {
				push(&node.Results[i], next)
			}
			for i := len(node.Params) - 1; i >= 0 && !stopped; i-- {
				push(&node.Params[i], next)
			}
			for i := len(node.TypeParams) - 1; i >= 0 && !stopped; i-- {
				push(&node.TypeParams[i], next)
			}
			if node.Receiver != nil {
				pushTracked(node.Receiver, next)
			}
		case *TypeParam:
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			addIdentifier(&node.NameID)
			push(&node.Constraint, next)
		case *Field:
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			addIdentifier(&node.NameID)
			push(&node.Type, next)
		case *TypeExpr:
			if node.Kind == TypeInvalid {
				continue
			}
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			switch node.Kind {
			case TypeName, TypeArray, TypeSlice, TypeMap, TypePointer, TypeFunc, TypeStruct, TypeInterface, TypeChan, TypeInstance:
			default:
				collector.Add(structureDiagnostic("ast.type.kind.unknown", "unknown type kind", node.Span))
			}
			addIdentifier(&node.NameID)
			addIdentifier(&node.QualifierID)
			for i := len(node.TypeArgs) - 1; i >= 0 && !stopped; i-- {
				push(&node.TypeArgs[i], next)
			}
			for i := len(node.Terms) - 1; i >= 0 && !stopped; i-- {
				push(&node.Terms[i], next)
			}
			for i := len(node.Embeds) - 1; i >= 0 && !stopped; i-- {
				push(&node.Embeds[i], next)
			}
			for i := len(node.Methods) - 1; i >= 0 && !stopped; i-- {
				push(&node.Methods[i], next)
			}
			for i := len(node.Fields) - 1; i >= 0 && !stopped; i-- {
				push(&node.Fields[i], next)
			}
			for i := len(node.Results) - 1; i >= 0 && !stopped; i-- {
				push(&node.Results[i], next)
			}
			for i := len(node.Params) - 1; i >= 0 && !stopped; i-- {
				push(&node.Params[i], next)
			}
			if node.Len != nil {
				pushTracked(node.Len, next)
			}
			if node.Key != nil {
				pushTracked(node.Key, next)
			}
			if node.Elem != nil {
				pushTracked(node.Elem, next)
			}
			if node.Base != nil {
				pushTracked(node.Base, next)
			}
		case *TypeTerm:
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			push(&node.Type, next)
		case *BlockStmt:
			if node.NodeID == 0 && !node.Span.Valid() && len(node.Stmts) == 0 {
				continue
			}
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			for i := len(node.Stmts) - 1; i >= 0 && !stopped; i-- {
				push(&node.Stmts[i], next)
			}
		case *Statement:
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			switch node.Kind {
			case StmtInvalid, StmtEmpty, StmtDecl, StmtExpr, StmtAssign, StmtReturn, StmtIf, StmtFor, StmtRange,
				StmtSwitch, StmtSelect, StmtSend, StmtBlock, StmtLabel, StmtBranch, StmtDefer, StmtGo, StmtPanic:
			default:
				collector.Add(structureDiagnostic("ast.stmt.kind.unknown", "unknown statement kind", node.Span))
			}
			addIdentifier(&node.LabelID)
			addIdentifier(&node.TypeSwitchID)
			for i := len(node.Results) - 1; i >= 0 && !stopped; i-- {
				push(&node.Results[i], next)
			}
			for i := len(node.Cases) - 1; i >= 0 && !stopped; i-- {
				push(&node.Cases[i], next)
			}
			if node.Range != nil {
				pushTracked(node.Range, next)
			}
			if node.Value != nil {
				pushTracked(node.Value, next)
			}
			if node.Key != nil {
				pushTracked(node.Key, next)
			}
			if node.Else != nil {
				pushTracked(node.Else, next)
			}
			if node.Post != nil {
				pushTracked(node.Post, next)
			}
			if node.Cond != nil {
				pushTracked(node.Cond, next)
			}
			if node.Init != nil {
				pushTracked(node.Init, next)
			}
			push(&node.Body, next)
			for i := len(node.Right) - 1; i >= 0 && !stopped; i-- {
				push(&node.Right[i], next)
			}
			for i := len(node.Left) - 1; i >= 0 && !stopped; i-- {
				push(&node.Left[i], next)
			}
			if node.Expr != nil {
				pushTracked(node.Expr, next)
			}
			for i := len(node.Decls) - 1; i >= 0 && !stopped; i-- {
				push(&node.Decls[i], next)
			}
		case *CaseClause:
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			push(&node.Body, next)
			if node.Comm != nil {
				pushTracked(node.Comm, next)
			}
			for i := len(node.Types) - 1; i >= 0 && !stopped; i-- {
				push(&node.Types[i], next)
			}
			for i := len(node.Values) - 1; i >= 0 && !stopped; i-- {
				push(&node.Values[i], next)
			}
		case *Expression:
			if node.Kind == ExprInvalid {
				continue
			}
			if !addNode(&node.NodeID, node.Span, item.depth) {
				continue
			}
			switch node.Kind {
			case ExprIdent, ExprLiteral, ExprUnary, ExprBinary, ExprCall, ExprSelector, ExprIndex, ExprIndexList,
				ExprSlice, ExprComposite, ExprFunc, ExprConvert, ExprAssert, ExprAddr, ExprDeref, ExprReceive, ExprEmbed:
			default:
				collector.Add(structureDiagnostic("ast.expr.kind.unknown", "unknown expression kind", node.Span))
			}
			addIdentifier(&node.NameID)
			if node.Func != nil {
				pushTracked(node.Func, next)
			} else if node.Kind == ExprFunc {
				collector.Add(structureDiagnostic("ast.expr.func.missing", "function expression requires a function body", node.Span))
			}
			for i := len(node.Items) - 1; i >= 0 && !stopped; i-- {
				push(&node.Items[i], next)
			}
			if node.Max != nil {
				pushTracked(node.Max, next)
			}
			if node.End != nil {
				pushTracked(node.End, next)
			}
			if node.Start != nil {
				pushTracked(node.Start, next)
			}
			if node.Index != nil {
				pushTracked(node.Index, next)
			}
			for i := len(node.Args) - 1; i >= 0 && !stopped; i-- {
				push(&node.Args[i], next)
			}
			if node.Callee != nil {
				pushTracked(node.Callee, next)
			}
			if node.Operand != nil {
				pushTracked(node.Operand, next)
			}
			if node.Right != nil {
				pushTracked(node.Right, next)
			}
			if node.Left != nil {
				pushTracked(node.Left, next)
			}
			if node.Type != nil && node.Type.Kind != TypeInvalid {
				pushTracked(node.Type, next)
			}
		case *KeyValue:
			if !addNode(&node.NodeID, node.Value.Span, item.depth) {
				continue
			}
			push(&node.Value, next)
			if node.Key != nil {
				pushTracked(node.Key, next)
			}
		}
	}
	for i := range fileStarts {
		end := nodes
		if i+1 < len(fileStarts) {
			end = fileStarts[i+1]
		}
		fileStarts[i] = end - fileStarts[i]
	}
	return collector.Diagnostics(), StructureStats{Nodes: nodes, FileNodes: fileStarts}
}

func normalizeStructureLimits(limits Limits) Limits {
	if limits.MaxDepth <= 0 || limits.MaxDepth > DefaultMaxDepth {
		limits.MaxDepth = DefaultMaxDepth
	}
	if limits.MaxNodes <= 0 || limits.MaxNodes > DefaultMaxNodes {
		limits.MaxNodes = DefaultMaxNodes
	}
	if limits.MaxDiagnostics <= 0 || limits.MaxDiagnostics > DefaultMaxDiagnostics {
		limits.MaxDiagnostics = DefaultMaxDiagnostics
	}
	return limits
}

func validateStructureSpan(span source.Span, fileEnds map[string]int, collector *source.DiagnosticCollector) {
	if span.Start.Offset == 0 && span.Start.Line == 0 && span.Start.Column == 0 &&
		span.End.Offset == 0 && span.End.Line == 0 && span.End.Column == 0 {
		return
	}
	if !span.Valid() {
		collector.Add(structureDiagnostic("ast.span.invalid", "AST span is invalid", span))
		return
	}
	end, exists := fileEnds[span.Start.File]
	if !exists || span.Start.Offset < 0 || end >= 0 && span.End.Offset > end {
		collector.Add(structureDiagnostic("ast.span.outside_file", "AST span lies outside its source file", span))
	}
}

func structureDiagnostic(code, message string, span source.Span) source.Diagnostic {
	return source.Diagnostic{Code: source.DiagnosticCode(code), Severity: source.SeverityError, Message: message, Primary: span}
}
