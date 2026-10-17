package optimize

import "github.com/d7z-team/mini-go/compiler/hir"

type functionCFG struct {
	labels       map[string]int
	successors   [][]int
	predecessors [][]int
}

func buildControlFlow(body []hir.Statement) functionCFG {
	cfg := functionCFG{labels: make(map[string]int), successors: make([][]int, len(body)), predecessors: make([][]int, len(body))}
	for index, statement := range body {
		if statement.Kind == hir.StmtLabel {
			cfg.labels[statement.Label] = index
		}
	}
	for index := range body {
		cfg.successors[index] = statementSuccessors(body, cfg.labels, index)
		for _, successor := range cfg.successors[index] {
			if successor >= 0 && successor < len(body) {
				cfg.predecessors[successor] = append(cfg.predecessors[successor], index)
			}
		}
	}
	return cfg
}
