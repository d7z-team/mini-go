package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestTasksShareOneModuleInitializationAcrossSchedulingQuanta(t *testing.T) {
	root, dependency := exportAddressArtifacts()
	read := root.Functions[0]
	read.ID = "fn.read"
	root.Functions = []ir.Function{
		{ID: "fn.entry", Signature: testSignature("function() Int64"), Code: testSlotCode([]string{"function() Void", "Int64"}, []ir.Instruction{
			{Op: ir.OpMakeClosure, Payload: ir.ClosurePayload{Function: "fn.worker"}},
			{Op: ir.OpSpawn, Payload: ir.CallPayload{}},
			{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.read", ResultCount: 1}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, {1}}, {{1}, nil}})},
		read,
		{ID: "fn.worker", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"Int64"}, []ir.Instruction{
			{Op: ir.OpCallDirect, Payload: ir.CallPayload{Function: "fn.read", ResultCount: 1}},
			{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.child"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}})},
	}
	root.Globals = append(root.Globals, ir.Global{ID: "global.child", Type: testType("Int64")})
	insertTestDelay(dependency.Functions[0].Code, 0, taskInstructionQuantum*2)
	program := patchMultiModuleProgram(t, root, dependency, "shared-init")
	for _, parallelism := range []int{1, 2, 4} {
		t.Run(strconv.Itoa(parallelism), func(t *testing.T) {
			instance, err := program.Instantiate(t.Context(), InstanceOptions{Parallelism: parallelism})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestInstance(t, instance)
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			result, err := execution.Wait(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if value, ok := result.Values[0].Int64(); !ok || value != 1 {
				t.Fatalf("parent observed partial initialization: %v", result.Values)
			}
			if _, err := execution.WaitScope(t.Context()); err != nil {
				t.Fatal(err)
			}
			value := instance.vm.rootModule().state.globals["global.child"].load()
			if value, ok := value.signedValue(); !ok || value != 1 {
				t.Fatalf("child observed partial initialization: %d", value)
			}
		})
	}
}

func TestModuleInitializationWaitsForAnotherTaskAndPropagatesFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			root, dependency := exportAddressArtifacts()
			instance, err := patchMultiModuleProgram(t, root, dependency, name).Instantiate(context.Background(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestInstance(t, instance)
			module, _ := instance.vm.moduleRegistry().module(dependency.Module.Path)
			machine := &executionMachine{vm: instance.vm}
			owner := &executionTask{id: 1}
			if err := machine.initializeModule(owner, nil, module, nil); err != nil {
				t.Fatal(err)
			}
			caller, err := instance.vm.newExecutionFrame(instance.vm.rootModule(), "fn.entry", nil, nil, 2, 1, false)
			if err != nil {
				t.Fatal(err)
			}
			waiter := &executionTask{id: 2, frames: []*executionFrame{caller}}
			defer machine.abortTask(waiter)
			defer machine.abortTask(owner)
			resumed := 0
			if err := machine.initializeModule(waiter, caller, module, func(*executionTask, *executionFrame) error {
				resumed++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if ready, err := machine.resumeBlocked(waiter, waiter.blocked); ready || err != nil || resumed != 0 {
				t.Fatalf("premature resume: ready=%t, resumed=%d, err=%v", ready, resumed, err)
			}
			failure := errors.New("initializer failed")
			if fail {
				module.state.finishInitialization(failure)
			} else {
				module.state.finishInitialization(nil)
			}
			machine.blocked = []*executionTask{waiter}
			waiter.ownership.enqueue()
			waiter.ownership.acquire()
			waiter.blocked.ticket = waiter.ownership.beginWait()
			waiter.ownership.relinquish()
			if err := machine.wakeBlocked(); err != nil {
				t.Fatal(err)
			}
			if machine.popRunnable() != waiter || waiter.blocked != nil || module.state.initTask != nil {
				t.Fatal("initialization did not return waiter ownership")
			}
			if fail {
				if !errors.Is(waiter.pendingErr, failure) || resumed != 0 {
					t.Fatalf("failed initializer delivered continuation: %v, %d", waiter.pendingErr, resumed)
				}
			} else if waiter.pendingErr != nil || resumed != 1 {
				t.Fatalf("successful initializer: %v, %d", waiter.pendingErr, resumed)
			}
		})
	}
}

func TestModuleInitializationPublishesOnlyAfterFramePreparation(t *testing.T) {
	root, dependency := exportAddressArtifacts()
	instance, err := patchMultiModuleProgram(t, root, dependency, "initialization-census").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	module, ok := instance.vm.moduleRegistry().module(dependency.Module.Path)
	if !ok {
		t.Fatal("dependency module is missing")
	}

	instance.vm.limits.MaxAllocatedBytes = 1
	if err := instance.vm.enterOwner(); err != nil {
		t.Fatal(err)
	}
	instance.vm.publishSlices(1)
	defer instance.vm.releaseSlice()
	task := &executionTask{id: 1}
	machine := &executionMachine{vm: instance.vm}
	err = machine.initializeModule(task, nil, module, nil)
	if _, ok := findGuestCensusRequest(err); !ok {
		t.Fatalf("frame preparation error = %v, want guest census", err)
	}
	if module.state.initState != moduleUninitialized || module.state.initTask != nil || len(task.frames) != 0 {
		t.Fatalf("failed preparation published initialization: state=%d task=%p frames=%d", module.state.initState, module.state.initTask, len(task.frames))
	}
}

func TestModuleInitializationDetectsRecursiveAndCrossTaskWaitCycles(t *testing.T) {
	a := &moduleInstance{executable: &executable{}, state: &moduleState{initState: moduleInitializing}}
	b := &moduleInstance{executable: &executable{}, state: &moduleState{initState: moduleInitializing}}
	first, second := &executionTask{id: 1}, &executionTask{id: 2}
	a.state.initTask, b.state.initTask = first, second
	first.blocked = &blockedOperation{kind: "module", module: b}
	machine := &executionMachine{}
	err := machine.initializeModule(second, &executionFrame{}, a, nil)
	if err == nil || !strings.Contains(err.Error(), "initialization cycle") || second.blocked != nil {
		t.Fatalf("cross-task cycle: %v", err)
	}
	first.blocked = nil
	err = machine.initializeModule(first, &executionFrame{}, a, nil)
	if err == nil || !strings.Contains(err.Error(), "initialization cycle") || first.blocked != nil {
		t.Fatalf("recursive initialization: %v", err)
	}
}
