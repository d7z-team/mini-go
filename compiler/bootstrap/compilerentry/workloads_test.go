package compilerentry

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler/cache"
)

func TestCompilerWorkloadsCheckAndAnalyze(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/language/workloads.json")
	if err != nil {
		t.Fatal(err)
	}
	var workloads []struct{ Name, Source string }
	if err := json.Unmarshal(data, &workloads); err != nil {
		t.Fatal(err)
	}
	for _, workload := range workloads {
		t.Run(workload.Name, func(t *testing.T) {
			packages := []Package{{Namespace: "module:sample", ModulePath: "sample", Files: []File{{Path: "main.mgo", Text: workload.Source}}}}
			compiler := NewService(cache.TransientConfig{})
			defer compiler.Close()
			started := time.Now()
			checked := compiler.Execute(Request{Operation: OperationCheck, Root: "sample", Packages: packages})
			if checked.Error != "" || len(checked.Diagnostics) != 0 {
				t.Fatalf("check: %s %+v", checked.Error, checked.Diagnostics)
			}
			t.Logf("check: %s", time.Since(started))
			var tools ToolService
			started = time.Now()
			opened := tools.Execute(t.Context(), ToolsRequest{Operation: "workspace/open", Root: "sample", Packages: packages})
			if opened.Error != nil || len(opened.Diagnostics) != 0 {
				t.Fatalf("open: %+v", opened)
			}
			defer tools.Execute(t.Context(), ToolsRequest{Operation: "workspace/close", Session: opened.Session})
			t.Logf("open: %s", time.Since(started))
			for range 2 {
				analyzed := tools.Execute(t.Context(), ToolsRequest{Operation: "workspace/analyze", Session: opened.Session, Revision: opened.Revision})
				if analyzed.Error != nil || analyzed.Analysis == nil || len(analyzed.Diagnostics) != 0 {
					t.Fatalf("analyze: %+v", analyzed)
				}
				if len(analyzed.Analysis.WorkspaceDiagnostics) != 0 {
					t.Fatal(analyzed.Analysis.WorkspaceDiagnostics)
				}
				for uri, report := range analyzed.Analysis.Diagnostics {
					if len(report.Items) != 0 {
						t.Fatalf("%s: %+v", uri, report.Items)
					}
				}
			}
		})
	}
}
