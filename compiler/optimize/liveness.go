package optimize

import "github.com/d7z-team/mini-go/compiler/hir"

func removeDeadPureLocalStoresOnce(body []hir.Statement, resultLocals []string) ([]hir.Statement, bool) {
	cfg := buildControlFlow(body)
	localIndex := make(map[string]int)
	addLocal := func(local string) {
		if local != "" {
			if _, ok := localIndex[local]; !ok {
				localIndex[local] = len(localIndex)
			}
		}
	}
	protected := make(map[string]struct{})
	for _, local := range resultLocals {
		addLocal(local)
		protected[local] = struct{}{}
	}
	uses := make([][]uint64, len(body))
	defs := make([][]uint64, len(body))
	for _, statement := range body {
		collectStatementProtectedLocals(statement, protected)
		collectStatementLocalNames(statement, addLocal)
	}
	words := (len(localIndex) + 63) / 64
	for index, statement := range body {
		uses[index] = make([]uint64, words)
		defs[index] = make([]uint64, words)
		collectStatementBits(statement, localIndex, uses[index], defs[index])
	}
	liveIn := make([][]uint64, len(body))
	liveOut := make([][]uint64, len(body))
	for index := range body {
		liveIn[index] = make([]uint64, words)
		liveOut[index] = make([]uint64, words)
	}
	for changed := true; changed; {
		changed = false
		for index := len(body) - 1; index >= 0; index-- {
			for word := 0; word < words; word++ {
				var out uint64
				for _, successor := range cfg.successors[index] {
					out |= liveIn[successor][word]
				}
				in := uses[index][word] | (out &^ defs[index][word])
				if liveOut[index][word] != out || liveIn[index][word] != in {
					liveOut[index][word], liveIn[index][word] = out, in
					changed = true
				}
			}
		}
	}
	protectedBits := make([]bool, len(localIndex))
	for local := range protected {
		if index, ok := localIndex[local]; ok {
			protectedBits[index] = true
		}
	}
	result := make([]hir.Statement, 0, len(body))
	var pending []hir.Location
	changed := false
	for index, statement := range body {
		if statement.Kind == hir.StmtStoreLocal && !statement.Rebind && isPureValueExpression(statement.Expr) {
			local := localIndex[statement.Local]
			if !protectedBits[local] && !hasBit(liveOut[index], local) {
				pending = appendUniqueLocations(pending, statement.SourcePoints)
				changed = true
				continue
			}
		}
		if len(pending) != 0 {
			statement.SourcePoints = prependUniqueLocations(pending, statement.SourcePoints)
			pending = nil
		}
		result = append(result, statement)
	}
	if len(pending) != 0 && len(result) != 0 {
		result[len(result)-1].SourcePoints = appendUniqueLocations(result[len(result)-1].SourcePoints, pending)
	}
	return result, changed
}

func collectStatementLocalNames(statement hir.Statement, add func(string)) {
	add(statement.Local)
	for _, target := range statement.Targets {
		add(target.Local)
	}
	collectStatementLocalReads(statement, add)
	for _, selected := range statement.SelectCases {
		add(selected.Channel)
		add(selected.Send)
		add(selected.Value)
		add(selected.OK)
	}
}

func collectStatementBits(statement hir.Statement, localIndex map[string]int, uses, defs []uint64) {
	collectStatementLocalReads(statement, func(local string) {
		if index, ok := localIndex[local]; ok {
			setBit(uses, index)
		}
	})
	if statement.Kind == hir.StmtStoreLocal {
		if index, ok := localIndex[statement.Local]; ok {
			setBit(defs, index)
		}
	}
	if statement.Kind == hir.StmtStoreResults || statement.Kind == hir.StmtStoreValues {
		for _, target := range statement.Targets {
			if target.Kind == "local" {
				if index, ok := localIndex[target.Local]; ok {
					setBit(defs, index)
				}
			}
		}
	}
}

func setBit(bits []uint64, index int) { bits[index/64] |= uint64(1) << (index % 64) }
func hasBit(bits []uint64, index int) bool {
	return index/64 < len(bits) && bits[index/64]&(uint64(1)<<(index%64)) != 0
}
