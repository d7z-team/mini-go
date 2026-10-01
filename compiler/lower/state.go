package lower

import (
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler/ast"
	ir "github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/types"
)

type lowerer struct {
	functions                  map[string]string
	methods                    map[string]methodInfo
	typeDecls                  map[string]ast.TypeExpr
	typeAliases                map[string]string
	extraFunctions             []ir.Function
	nextAnonFunc               int
	constants                  map[string]string
	constValues                map[string]constantValue
	globals                    map[string]string
	globalTypes                map[string]types.TypeRef
	globalVariadics            map[string]bool
	packageSymbols             map[string]struct{}
	imports                    map[string]string
	importsByFile              map[string]map[string]string
	unambiguousDotImports      map[string]moduleExportInfo
	dotImportsByFile           map[string]map[string]moduleExportInfo
	importPaths                []string
	implicitImports            map[string]struct{}
	importExports              map[string]map[string]struct{}
	moduleExports              map[string]map[string]moduleExportInfo
	importedTypes              map[string]moduleExportInfo
	diagnostics                []source.Diagnostic
	nextLabel                  int
	nextSyntheticLocal         int
	nextConst                  int
	branches                   []branchTarget
	labeledBranches            map[string][]branchTarget
	modulePath                 string
	program                    *ir.Program
	typeTable                  *types.TypeTable
	typeParser                 *types.Parser
	typeRefs                   map[string]types.TypeRef
	semanticTypeText           map[types.TypeRef]string
	resolvedTypes              map[string]string
	namedUnderlyingTypes       map[string]string
	relations                  types.Relations
	semantic                   *check.ProgramInfo
	suppressConstantArithmetic int
}

type constantValue struct {
	Type    string
	Value   json.RawMessage
	Untyped bool
}

type funcScope struct {
	locals            map[string]string
	localTypes        map[string]types.TypeRef
	localVariadics    map[string]bool
	funcValueResults  map[string]int
	funcValueTypes    map[string][]types.TypeRef
	funcValueParams   map[string][]types.TypeRef
	funcValueVarargs  map[string]bool
	namedResults      []ir.Expression
	namedResultLocals map[string]string
	resultTypes       []types.TypeRef
	expectedReturns   int
	constants         map[string]string
	constValues       map[string]constantValue
	futureConsts      map[string]int
	outer             *funcScope
	function          *ir.Function
	upvalues          map[string]string
	upvalueTypes      map[string]types.TypeRef
	upvalueVariadics  map[string]bool
	captures          map[string]ir.CaptureTarget
	rangeReturn       *rangeFunctionReturnContext
	deferOwnerDepth   int
	debugScope        int
}

type rangeFunctionReturnContext struct {
	expectedReturns   int
	resultTypes       []types.TypeRef
	namedResultLocals map[string]string
	resultTargets     []ir.StoreTarget
	returnFlagUpvalue string
	stopLabel         string
}

type methodInfo struct {
	ModulePath string
	FunctionID string
	Receiver   types.TypeRef
	Signature  types.FunctionSignature
}

type interfaceMethodInfo struct {
	InterfaceType types.TypeRef
	Method        string
	Signature     types.FunctionSignature
}

type moduleExportInfo struct {
	ModulePath string
	Kind       check.ObjectKind
	Type       string
	Underlying string
	Value      json.RawMessage
	Fields     []check.DependencyTypeField
	Methods    []check.DependencyTypeMethod
	Variadic   bool
	Untyped    bool
}

type addressLet struct {
	local string
	value ir.Expression
}

type addressTarget struct {
	expr ir.Expression
	lets []addressLet
}

type branchTarget struct {
	breakLabel       string
	continueLabel    string
	fallthroughLabel string
	userLabel        string
}

func (l *lowerer) add(code, message string, span source.Span) {
	l.diagnostics = append(l.diagnostics, source.Diagnostic{
		Code:     source.DiagnosticCode(code),
		Severity: "error",
		Message:  message,
		Primary:  span,
	})
}

func newFuncScope(fn *ir.Function, outer *funcScope) funcScope {
	debugScope := 0
	if fn != nil {
		debugScope = len(fn.DebugScopes) + 1
		parent := 0
		if outer != nil && outer.function == fn {
			parent = outer.debugScope
		}
		fn.DebugScopes = append(fn.DebugScopes, ir.DebugScope{ID: debugScope, Parent: parent})
	}
	return funcScope{
		locals:            map[string]string{},
		localTypes:        map[string]types.TypeRef{},
		localVariadics:    map[string]bool{},
		funcValueResults:  map[string]int{},
		funcValueTypes:    map[string][]types.TypeRef{},
		funcValueParams:   map[string][]types.TypeRef{},
		funcValueVarargs:  map[string]bool{},
		namedResults:      nil,
		namedResultLocals: map[string]string{},
		resultTypes:       nil,
		expectedReturns:   0,
		constants:         map[string]string{},
		constValues:       map[string]constantValue{},
		outer:             outer,
		function:          fn,
		upvalues:          map[string]string{},
		upvalueTypes:      map[string]types.TypeRef{},
		upvalueVariadics:  map[string]bool{},
		captures:          map[string]ir.CaptureTarget{},
		debugScope:        debugScope,
	}
}
