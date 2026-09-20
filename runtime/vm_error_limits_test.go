package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestVMRejectsMissingModuleRequirement(t *testing.T) {
	artifact := ir.NewArtifact("example/main", "main")
	artifact.Requirements = []ir.Requirement{{
		Kind:       "source",
		ModulePath: "example/missing",
		Exports:    []string{"Value"},
	}}
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
	}}

	_, err := loadTestEngineWithOptions(artifact, InstanceOptions{modules: newModuleRegistry()})
	if err == nil {
		t.Fatal("expected missing module error")
	}
}

func TestVMRejectsMissingDependencyExport(t *testing.T) {
	dependency := ir.NewArtifact("example/lib", "lib")
	executable, err := newLoader().load(dependency)
	if err != nil {
		t.Fatal(err)
	}
	modules := newModuleRegistry()
	if err := modules.addExecutable(executable); err != nil {
		t.Fatal(err)
	}
	root := ir.NewArtifact("example/main", "main")
	root.Requirements = []ir.Requirement{{Kind: ir.RequirementSource, ModulePath: "example/lib", Exports: []string{"Missing"}}}
	_, err = loadTestEngineWithOptions(root, InstanceOptions{modules: modules})
	if err == nil || !strings.Contains(err.Error(), `missing export "Missing"`) {
		t.Fatalf("load error: %v", err)
	}
}

func TestVMRejectsModuleHashMismatch(t *testing.T) {
	libArtifact := ir.NewArtifact("example/lib", "lib")
	libArtifact.Functions = []ir.Function{{
		ID:        "fn.value",
		Signature: testSignature("function() Void"),
	}}
	libArtifact.Exports = []ir.Export{{Name: "Value", Kind: "function", ID: "fn.value"}}
	modules := newModuleRegistry()
	libExecutable, err := newLoader().load(libArtifact)
	if err != nil {
		t.Fatalf("load dependency artifact failed: %v", err)
	}
	if err := modules.addExecutable(libExecutable); err != nil {
		t.Fatalf("AddArtifact failed: %v", err)
	}
	mainArtifact := ir.NewArtifact("example/main", "main")
	mainArtifact.Requirements = []ir.Requirement{{
		Kind:       "source",
		ModulePath: "example/lib",
		Hash:       "not-the-module-hash",
		Exports:    []string{"Value"},
	}}
	mainArtifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
	}}

	_, err = loadTestEngineWithOptions(mainArtifact, InstanceOptions{modules: modules})
	if err == nil {
		t.Fatal("expected module hash mismatch error")
	}
}

func TestVMRejectsModuleRequirementCycle(t *testing.T) {
	dependency := ir.NewArtifact("example/dependency", "dependency")
	dependency.Requirements = []ir.Requirement{{Kind: ir.RequirementSource, ModulePath: "example/main"}}
	dependency.Functions = []ir.Function{{ID: "fn.dependency", Signature: testSignature("function() Void")}}
	modules := newModuleRegistry()
	executable, err := newLoader().load(dependency)
	if err != nil {
		t.Fatal(err)
	}
	if err := modules.addExecutable(executable); err != nil {
		t.Fatal(err)
	}
	root := ir.NewArtifact("example/main", "main")
	root.Requirements = []ir.Requirement{{Kind: ir.RequirementSource, ModulePath: "example/dependency"}}
	root.Functions = []ir.Function{{ID: "fn.main", Signature: testSignature("function() Void")}}
	if _, err := loadTestEngineWithOptions(root, InstanceOptions{modules: modules}); err == nil || !strings.Contains(err.Error(), "module dependency cycle") {
		t.Fatalf("module cycle error = %v", err)
	}
}

func TestVMNilChannelReceiveReportsBlocked(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Instructions: []ir.Instruction{{
			Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int64>")}),
		}, {
			Op: string(ir.OpWaitableRecv),
		}, {
			Op:      string(ir.OpReturn),
			Payload: json.RawMessage(`{"result_count":1}`),
		}},
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	_, err = runTestModuleExport(vm, "Main")
	if err == nil {
		t.Fatal("expected wait blocked error")
	}
	if _, ok := err.(Error); !ok {
		t.Fatalf("expected Error, got %T: %v", err, err)
	}
	var blocked AllBlockedError
	if !errors.As(err, &blocked) || blocked.Total != 1 || len(blocked.Contexts) != 1 {
		t.Fatalf("blocked context = %#v", blocked)
	}
}

func TestVMReportsEveryBlockedExecutionContext(t *testing.T) {
	wait := []ir.Instruction{
		{Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int>")})},
		{Op: string(ir.OpWaitableRecv)},
		{Op: string(ir.OpPop)},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	}
	artifact := ir.NewArtifact("example/all-blocked", "main")
	artifact.Functions = []ir.Function{
		{
			ID: "fn.main", Signature: testSignature("function() Void"),
			Instructions: append([]ir.Instruction{
				{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.child"})},
				{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
			}, wait...),
		},
		{ID: "fn.child", Signature: testSignature("function() Void"), Instructions: wait},
	}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runTestModuleExport(vm, "Main")
	var blocked AllBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("run error = %T: %v", err, err)
	}
	if blocked.Total != 2 || len(blocked.Contexts) != 2 {
		t.Fatalf("blocked contexts = %#v", blocked)
	}
	if blocked.Contexts[0].ExecutionContextID >= blocked.Contexts[1].ExecutionContextID {
		t.Fatalf("blocked contexts are not deterministic: %#v", blocked.Contexts)
	}
	projected, projectionErr := ProjectExecutionError(err)
	if projectionErr != nil || projected.Code != "execution.blocked" || len(projected.Stack) != 2 {
		t.Fatalf("projected deadlock = %#v, %v", projected, projectionErr)
	}
}

func TestBlockedContextSnapshotIsSortedAndBounded(t *testing.T) {
	machine := &executionMachine{}
	for id := int64(70); id >= 1; id-- {
		machine.blocked = append(machine.blocked, &executionTask{
			id:    id,
			scope: &executionScope{id: id + 100},
			blocked: &blockedOperation{error: Error{
				ExecutionContextID: id, Generation: 2, ProgramHash: "revision", ModulePath: "example/blocked",
				FunctionID: "fn.wait", PC: 3, Op: string(ir.OpWaitableRecv), Err: WaitBlockedError{Message: "waiting"},
			}},
		})
	}
	total, contexts := machine.blockedContextSnapshot()
	if total != 70 || len(contexts) != maxBlockedContexts {
		t.Fatalf("blocked snapshot = total %d, contexts %d", total, len(contexts))
	}
	for index, context := range contexts {
		if context.ExecutionContextID != int64(index+1) || context.ScopeID != int64(index+101) || context.Reason != "waiting" {
			t.Fatalf("blocked context %d = %#v", index, context)
		}
	}
}

func TestLibraryIdleDoesNotReportForegroundDeadlock(t *testing.T) {
	artifact := ir.NewArtifact("example/library-idle", "main")
	artifact.Functions = []ir.Function{{
		ID: "fn.main", Signature: testSignature("function() Int64"),
		Instructions: []ir.Instruction{
			{Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int>")})},
			{Op: string(ir.OpWaitableRecv)},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{ResultCount: 1})},
		},
	}}
	vm, err := loadTestEngine(artifact)
	if err != nil {
		t.Fatal(err)
	}
	scopeID := vm.beginRun()
	if err := vm.prepareScheduledFunction(vm.rootModule(), "fn.main", nil, vm.nextSpawnExecutionContextID(), scopeID, false); err != nil {
		t.Fatal(err)
	}
	vm.machine.foreground = nil
	outcome := vm.machine.run(0)
	if outcome.state != ExecutionPending || outcome.err != nil {
		t.Fatalf("library idle = %+v", outcome)
	}
	vm.finishRun()
}

func TestVMEnforcesStepLimitInsideLoopAndKeepsLibraryOpen(t *testing.T) {
	artifact := ir.NewArtifact("example/module", "main")
	artifact.Functions = []ir.Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []ir.Instruction{{
			Op:      string(ir.OpLabel),
			Payload: json.RawMessage(`{"label":"loop"}`),
		}, {
			Op:      string(ir.OpJump),
			Payload: json.RawMessage(`{"label":"loop"}`),
		}},
	}}
	artifact.Exports = []ir.Export{{Name: "Main", Kind: "function", ID: "fn.main"}}

	vm, err := loadTestEngineWithOptions(artifact, InstanceOptions{Limits: Limits{MaxSteps: 3}})
	if err != nil {
		t.Fatalf("load test engine failed: %v", err)
	}
	execution, err := startTestExecution(vm, "Main")
	if err != nil {
		t.Fatalf("start execution failed: %v", err)
	}
	err = execution.Err()
	if err == nil {
		t.Fatal("expected step limit error")
	}
	var stepErr StepLimitError
	if !errors.As(err, &stepErr) {
		t.Fatalf("expected StepLimitError, got %T: %v", err, err)
	}
	if stepErr.MaxSteps != 3 {
		t.Fatalf("expected max steps 3, got %d", stepErr.MaxSteps)
	}
	if !execution.instance.isOpen() || execution.instance.Err() != nil {
		t.Fatalf("instance after scoped step limit: state=%s err=%v", execution.instance.State(), execution.instance.Err())
	}
}

func TestCollectionLimitsFailBeforeMutation(t *testing.T) {
	machine := &vm{limits: Limits{MaxCollectionElements: 1, MaxAllocatedBytes: 1 << 20}}
	module := &moduleInstance{vm: machine}
	if _, err := makeSliceValue(module, "Slice<Int>", newVMValue("Int", int64(2)), vmValue{}); err == nil {
		t.Fatal("oversized slice succeeded")
	} else {
		var limit ResourceLimitError
		if !errors.As(err, &limit) || limit.Code != "execution.collection_limit" {
			t.Fatalf("make slice error = %T %v", err, err)
		}
	}
	if allocated := machine.totalAllocatedBytes.Load(); allocated != 0 {
		t.Fatalf("failed slice charged %d bytes", allocated)
	}

	slice := newSliceValue("Slice<Int>", []vmValue{newVMValue("Int", int64(1))})
	if _, err := appendValue(module, slice, []vmValue{newVMValue("Int", int64(2))}, false); err == nil {
		t.Fatal("oversized append succeeded")
	}
	header := slice.Data.(*vmSlice)
	if header.Len != 1 || header.valueAt(0).materializedData() != int64(1) {
		t.Fatalf("failed append mutated slice: %#v", header)
	}

	data := newVMMap(1)
	object := newVMValue("Map<Int, Int>", data)
	if _, err := setIndexValue(module, object, newVMValue("Int", int64(1)), newVMValue("Int", int64(1))); err != nil {
		t.Fatal(err)
	}
	if _, err := setIndexValue(module, object, newVMValue("Int", int64(2)), newVMValue("Int", int64(2))); err == nil {
		t.Fatal("oversized map store succeeded")
	}
	if data.length() != 1 {
		t.Fatalf("failed map store mutated map: %#v", data.snapshot())
	}
}
