package emit

import (
	"strconv"

	"github.com/d7z-team/mini-go/compiler/hir"
)

// Close lexical iterators crossed by goto, labeled break, or outer continue.
// Returns and panic unwind their frame, which owns any remaining iterators.
func closeExitedMapIterators(body []hir.Statement) []hir.Statement {
	type interval struct {
		local      string
		start, end int
	}
	var ranges []interval
	starts := make(map[string]int)
	labels := make(map[string]int)
	for index, stmt := range body {
		switch stmt.Kind {
		case hir.StmtMapIterInit:
			starts[stmt.Local] = index
		case hir.StmtMapIterClose:
			if start, ok := starts[stmt.Local]; ok {
				ranges = append(ranges, interval{stmt.Local, start, index})
			}
		case hir.StmtLabel:
			labels[stmt.Label] = index
		}
	}
	if len(ranges) == 0 {
		return body
	}
	out := make([]hir.Statement, 0, len(body))
	for index, stmt := range body {
		if stmt.Kind != hir.StmtJump && stmt.Kind != hir.StmtJumpIf {
			out = append(out, stmt)
			continue
		}
		target, known := labels[stmt.Label]
		var closes []hir.Statement
		if known {
			for _, r := range ranges {
				if index > r.start && index < r.end && (target <= r.start || target > r.end) {
					closes = append(closes, hir.Statement{Kind: hir.StmtMapIterClose, Local: r.local})
				}
			}
		}
		if len(closes) != 0 && stmt.Kind == hir.StmtJumpIf {
			cleanup := "map.cleanup." + strconv.Itoa(index)
			after := cleanup + ".after"
			branch := stmt
			branch.Label = cleanup
			out = append(out, branch, hir.Statement{Kind: hir.StmtJump, Label: after}, hir.Statement{Kind: hir.StmtLabel, Label: cleanup})
			out = append(out, closes...)
			out = append(out, hir.Statement{Kind: hir.StmtJump, Label: stmt.Label}, hir.Statement{Kind: hir.StmtLabel, Label: after})
			continue
		}
		out = append(out, closes...)
		out = append(out, stmt)
	}
	return out
}
