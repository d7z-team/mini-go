package lower

import (
	"sort"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/hir"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func testDependencies(members []check.DependencyExport) []check.DependencyPackage {
	packages := make(map[string][]check.DependencyExport)
	for _, member := range members {
		packages[member.ModulePath] = append(packages[member.ModulePath], member)
	}
	paths := make([]string, 0, len(packages))
	for path := range packages {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var result []check.DependencyPackage
	for _, path := range paths {
		result = append(result, check.DependencyPackage{ModulePath: path, Members: packages[path]})
	}
	return result
}

func lowerTestProgram(program ast.Program) (hir.Program, []source.Diagnostic) {
	return lowerTestProgramWithOptions(program, Options{})
}

func lowerTestProgramWithOptions(program ast.Program, options Options) (hir.Program, []source.Diagnostic) {
	checked := check.WithOptions(program, check.AnalyzeOptions{Dependencies: options.Dependencies})
	return Lower(checked, options)
}

func findInstruction(instructions []ir.Instruction, op ir.Opcode) (ir.Instruction, bool) {
	for _, instruction := range instructions {
		if instruction.Op == op {
			return instruction, true
		}
	}
	return ir.Instruction{}, false
}

func countStatements(statements []hir.Statement, kind hir.StatementKind) int {
	count := 0
	for _, statement := range statements {
		if statement.Kind == kind {
			count++
		}
	}
	return count
}

func hasAddressPath(statements []hir.Statement, kind string) bool {
	for _, statement := range statements {
		address := statement.Expr
		if statement.Kind == hir.StmtStoreIndirect {
			address = statement.Object
		}
		if address.Kind != hir.ExprAddressOf {
			continue
		}
		for _, segment := range address.Path {
			if segment.Kind == kind {
				return true
			}
		}
	}
	return false
}

func findFunction(functions []hir.Function, id string) (hir.Function, bool) {
	for _, fn := range functions {
		if fn.ID == id {
			return fn, true
		}
	}
	return hir.Function{}, false
}

func findIRFunction(functions []ir.Function, id string) (ir.Function, bool) {
	for _, fn := range functions {
		if fn.ID == id {
			return fn, true
		}
	}
	return ir.Function{}, false
}

func ptrExpr(expr ast.Expression) *ast.Expression {
	return &expr
}

func requireDiagnostic(t *testing.T, diagnostics []source.Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if string(diagnostic.Code) == code {
			return
		}
	}
	t.Fatalf("expected diagnostic %q, got %#v", code, diagnostics)
}
