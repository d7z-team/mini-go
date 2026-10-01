package parser

import (
	"strconv"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/source"
)

func TestNestedCompositeStructureTracksSourceOccurrences(t *testing.T) {
	text := "package sample\nvar value = " + strings.Repeat("[]any{", 12) + "1" + strings.Repeat("}", 12)
	result := ParseFileWithLimits("sample", source.NewFile("main", "main.mgo", text), Limits{MaxASTNodes: 128})
	if source.HasErrors(result.Diagnostics) {
		t.Fatalf("small nested composite exceeded its source budget: %v", result.Diagnostics)
	}
	var composites, literals int
	ast.WalkExpressions(&result.Program, func(expression *ast.Expression) {
		switch expression.Kind {
		case ast.ExprComposite:
			composites++
		case ast.ExprLiteral:
			literals++
		}
	})
	if composites != 12 || literals != 1 {
		t.Fatalf("visits differ from source occurrences: %d composites, %d literals", composites, literals)
	}
}

func BenchmarkNestedCompositeParse(b *testing.B) {
	for _, depth := range []int{4, 8, 12} {
		b.Run(strconv.Itoa(depth), func(b *testing.B) {
			text := "package sample\nvar value = " + strings.Repeat("[]any{", depth) + "1" + strings.Repeat("}", depth)
			file := source.NewFile("main", "main.mgo", text)
			b.ReportAllocs()
			var result Result
			for b.Loop() {
				result = ParseFile("sample", file)
				if source.HasErrors(result.Diagnostics) {
					b.Fatal(result.Diagnostics)
				}
			}
			b.ReportMetric(float64(result.NodeCount), "nodes/op")
		})
	}
}
