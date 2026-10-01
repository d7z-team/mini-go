package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/bootstrap/compilerentry"
	"github.com/d7z-team/mini-go/compiler/language"
	"github.com/d7z-team/mini-go/compiler/service"
	miniruntime "github.com/d7z-team/mini-go/runtime"
)

func TestToolsImageMatchesNativeWorkspace(t *testing.T) {
	image := buildCompilerImage(t)
	instance := instantiateCompilerImage(t, image)
	var native compilerentry.ToolService
	data, err := os.ReadFile("../../testdata/language/workspace.json")
	if err != nil {
		t.Fatal(err)
	}
	var request compilerentry.ToolsRequest
	if err = json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	request.Operation = "workspace/open"
	var nativeSession, guestSession, nativeRevision, guestRevision string
	call := func(request compilerentry.ToolsRequest) compilerentry.ToolsResponse {
		t.Helper()
		request.Format = compilerentry.ToolsFormat
		request.Version = compilerentry.ToolsVersion
		bytes, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		result, err := instance.Call(t.Context(), "tools", miniruntime.HostBytes(bytes))
		if err != nil {
			t.Fatal(err)
		}
		output, ok := result.Values[0].Bytes()
		if !ok {
			t.Fatal("expected bytes")
		}
		response, err := compilerentry.DecodeToolsResponse(output)
		if err != nil {
			t.Fatal(err)
		}
		if response.Error != nil {
			t.Fatal(response.Error)
		}
		return response
	}
	opened := native.Execute(t.Context(), request)
	if opened.Error != nil {
		t.Fatal(opened.Error)
	}
	nativeSession, nativeRevision = opened.Session, opened.Revision
	opened = call(request)
	guestSession, guestRevision = opened.Session, opened.Revision
	defer native.Execute(t.Context(), compilerentry.ToolsRequest{Operation: "workspace/close", Session: nativeSession})
	nativeAnalysis := native.Execute(t.Context(), compilerentry.ToolsRequest{Operation: "workspace/analyze", Session: nativeSession, Revision: nativeRevision})
	guestAnalysis := call(compilerentry.ToolsRequest{Operation: "workspace/analyze", Session: guestSession, Revision: guestRevision})
	for name, pair := range map[string]struct{ session, snapshot string }{"native": {nativeSession, nativeAnalysis.Analysis.Snapshot}, "guest": {guestSession, guestAnalysis.Analysis.Snapshot}} {
		request = compilerentry.ToolsRequest{Operation: "language/hover", Session: pair.session, Query: service.Query{Snapshot: pair.snapshot, URI: "mini-go://sample/main.mgo", Position: language.Position{Line: 2, Character: 6}}}
		var response compilerentry.ToolsResponse
		if name == "native" {
			response = native.Execute(t.Context(), request)
		} else {
			response = call(request)
		}
		encoded, _ := json.Marshal(response.Value)
		if response.Error != nil || !strings.Contains(string(encoded), "Answer") {
			t.Fatalf("%s hover: %+v", name, response)
		}
	}
	data, err = os.ReadFile("../../testdata/language/queries.json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []service.Query
	if err = json.Unmarshal(data, &queries); err != nil {
		t.Fatal(err)
	}
	for _, query := range queries {
		query.URI = "mini-go://sample/main.mgo"
		query.Snapshot = nativeAnalysis.Analysis.Snapshot
		nativeResult := native.Execute(t.Context(), compilerentry.ToolsRequest{Operation: "language/" + query.Operation, Session: nativeSession, Query: query})
		query.Snapshot = guestAnalysis.Analysis.Snapshot
		guestResult := call(compilerentry.ToolsRequest{Operation: "language/" + query.Operation, Session: guestSession, Query: query})
		left, _ := json.Marshal(nativeResult.Value)
		right, _ := json.Marshal(guestResult.Value)
		var normalized any
		if err = json.Unmarshal(left, &normalized); err != nil {
			t.Fatal(err)
		}
		left, _ = json.Marshal(normalized)
		if nativeResult.Error != nil || !bytes.Equal(left, right) {
			t.Fatalf("%s: native %s (%v), guest %s", query.Operation, left, nativeResult.Error, right)
		}
	}
	call(compilerentry.ToolsRequest{Operation: "workspace/close", Session: guestSession})
	data, err = os.ReadFile("../../testdata/workspace/sources.json")
	if err != nil {
		t.Fatal(err)
	}
	var resolution compilerentry.ToolsRequest
	if err = json.Unmarshal(data, &resolution); err != nil {
		t.Fatal(err)
	}
	resolution.Operation = "workspace/sources"
	nativeResolved := native.Execute(t.Context(), resolution)
	guestResolved := call(resolution)
	left, _ := json.Marshal(nativeResolved.Value)
	right, _ := json.Marshal(guestResolved.Value)
	var normalized any
	if err = json.Unmarshal(left, &normalized); err != nil {
		t.Fatal(err)
	}
	left, _ = json.Marshal(normalized)
	if nativeResolved.Error != nil || !bytes.Equal(left, right) {
		t.Fatalf("source assembly differs: %s (%v) != %s", left, nativeResolved.Error, right)
	}
}
