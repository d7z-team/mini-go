package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestScopeCancellationReclaimsHandoffTaskAndItsReservedBudget(t *testing.T) {
	machine := &executionMachine{vm: &vm{}}
	first := machine.newScope(1, 1, false, nil)
	second := machine.newScope(2, 2, false, nil)
	task := &executionTask{id: 1, scope: first, budget: first.budget}
	other := &executionTask{id: 2, scope: second, budget: second.budget}
	machine.addTask(task)
	machine.pushRunnable(task)
	machine.addTask(other)
	machine.pushRunnable(other)
	if machine.popRunnable() != task || !task.ownership.acquire() {
		t.Fatal("handoff task did not acquire its lease")
	}
	if err := task.consumeStep(64); err != nil {
		t.Fatal(err)
	}
	ticket := task.ownership.beginWait()
	task.ownership.relinquish()
	machine.cancelScope(first, context.Canceled)
	if !first.settled || first.tasks != 0 || first.budget.reserved != 0 || first.budget.steps.Load() != 1 {
		t.Fatal("canceled handoff retained task or instruction grant")
	}
	if task.ownership.notify(ticket) || !task.ownership.terminal() {
		t.Fatal("canceled handoff accepted delayed completion")
	}
	if second.settled || machine.tasks[2] != other || machine.popRunnable() != other {
		t.Fatal("scope cancellation disturbed an independent task")
	}
	machine.cancelScope(second, context.Canceled)
	if len(machine.tasks) != 0 {
		t.Fatal("canceled scopes retained task registrations")
	}
}

func TestEntryAdmissionCountsBackgroundTasksBeforeAllocation(t *testing.T) {
	artifact := ir.NewArtifact("scheduler/task-admission", "main")
	var worker []ir.Instruction
	for range taskInstructionQuantum {
		worker = append(worker, ir.Instruction{Op: string(ir.OpZero), Payload: testTypePayload("Bool")}, ir.Instruction{Op: string(ir.OpPop)})
	}
	wait := []ir.Instruction{{Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int>")})}, {Op: string(ir.OpWaitableRecv)}, {Op: string(ir.OpPop)}}
	worker = append(worker,
		ir.Instruction{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.child"})},
		ir.Instruction{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
	)
	worker = append(worker, wait...)
	artifact.Functions = []ir.Function{
		{ID: "fn.entry", Signature: testSignature("function() Void"), Instructions: []ir.Instruction{
			{Op: string(ir.OpMakeClosure), Payload: testPayload(ir.ClosurePayload{Function: "fn.worker"})},
			{Op: string(ir.OpSpawn), Payload: testPayload(ir.CallPayload{})},
			{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
		}},
		{ID: "fn.worker", Signature: testSignature("function() Void"), Instructions: worker},
		{ID: "fn.child", Signature: testSignature("function() Void"), Instructions: wait},
	}
	instance, err := patchTestProgram(t, artifact, "task-admission").Instantiate(t.Context(), InstanceOptions{Limits: Limits{MaxTasks: 2}})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	instance.supervisor.Load().stop()
	execution, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	outcome := instance.driveTasks(t.Context(), taskInstructionQuantum*4, true)
	if err := instance.vm.enterOwnerContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	blocked := len(instance.vm.machine.blocked)
	before := instance.vm.totalAllocatedBytes.Load()
	instance.vm.leaveOwner()
	if outcome.state != ExecutionPending || outcome.err != nil || blocked != 2 {
		t.Fatalf("background tasks not parked: state=%s blocked=%d err=%v", outcome.state, blocked, outcome.err)
	}
	_, err = instance.Start("run")
	var limit ResourceLimitError
	if !errors.As(err, &limit) || limit.Code != "execution.task_limit" {
		t.Fatalf("entry exceeded task limit: %v", err)
	}
	if instance.vm.totalAllocatedBytes.Load() != before {
		t.Fatal("rejected task allocated a frame")
	}
}

func TestTaskHandoffKeepsCensusAndRevisionRoots(t *testing.T) {
	instance, err := patchTestProgram(t, patchGlobalArtifact(1), "task-handoff-roots").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Start("run"); err != nil {
		t.Fatal(err)
	}
	machine := instance.vm.machine
	task := machine.popRunnable()
	if task == nil || !task.ownership.acquire() {
		t.Fatal("task not available for handoff")
	}
	defer func() {
		task.ownership.relinquish()
		machine.pushRunnable(task)
	}()
	const bytes = 65536
	task.frames[0].frame.push(newByteSliceHeaderValue("Slice<Uint8>", make([]byte, bytes), bytes, bytes))
	if retained := instance.vm.refreshLiveGuestBytes(); retained < bytes {
		t.Fatalf("task outside queues lost its guest roots: %d", retained)
	}
	roots, err := instance.RevisionRoots(t.Context(), 1, RevisionRootLimits{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, root := range roots.Roots {
		found = found || (root.TaskID == task.id && strings.HasPrefix(root.Path, "task "))
	}
	if !found || !roots.Complete {
		t.Fatalf("task outside queues lost its revision root: %+v", roots)
	}
}

func TestReturningFrameKeepsValuesRootedUntilContinuationAcceptsThem(t *testing.T) {
	artifact := ir.NewArtifact("scheduler/return-roots", "main")
	artifact.Functions = []ir.Function{{ID: "fn.entry", Signature: testSignature("function() Slice<Uint8>"), Instructions: []ir.Instruction{
		{Op: string(ir.OpZero), Payload: testTypePayload("Slice<Uint8>")},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{ResultCount: 1})},
	}}}
	instance, err := patchTestProgram(t, artifact, "return-roots").Instantiate(t.Context(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if _, err := instance.Start("run"); err != nil {
		t.Fatal(err)
	}
	machine := instance.vm.machine
	task := machine.popRunnable()
	if !task.ownership.acquire() {
		t.Fatal("task lease unavailable")
	}
	defer func() {
		task.ownership.relinquish()
		machine.abortTask(task)
		machine.finishTask(task, nil)
	}()
	const bytes = 65536
	current := task.frames[0]
	current.completion = &frameCompletion{returnValues: []vmValue{newByteSliceHeaderValue("Slice<Uint8>", make([]byte, bytes), bytes, bytes)}}
	var retained int64
	current.resume = func(*executionTask, *executionFrame, []vmValue) error {
		retained = instance.vm.refreshLiveGuestBytes()
		return nil
	}
	values, done, err := machine.advanceCompletion(task)
	if err != nil || !done || len(values) != 1 {
		t.Fatalf("return completion: done=%t values=%d err=%v", done, len(values), err)
	}
	if retained < bytes {
		t.Fatalf("return continuation lost guest roots before accepting values: %d", retained)
	}
}

func TestTaskAdmissionIncludesExecutionLeasesAndWaitHandoffs(t *testing.T) {
	machine := &executionMachine{vm: &vm{limits: Limits{MaxTasks: 1}}}
	task := &executionTask{id: 1}
	machine.addTask(task)
	machine.pushRunnable(task)
	if machine.popRunnable() != task || !task.ownership.acquire() {
		t.Fatal("task did not acquire execution lease")
	}
	for _, park := range []bool{false, true} {
		if park {
			task.ownership.beginWait()
		}
		err := machine.admitTask()
		var limit ResourceLimitError
		if !errors.As(err, &limit) || limit.Code != "execution.task_limit" {
			t.Fatalf("task outside ready queue escaped admission: %v", err)
		}
	}
	task.ownership.relinquish()
	machine.finishTask(task, nil)
	if err := machine.admitTask(); err != nil || len(machine.tasks) != 0 {
		t.Fatalf("completed task retained admission: tasks=%d err=%v", len(machine.tasks), err)
	}
}

func TestTaskLeaseAllowsOnlyOneWorkerAndDeduplicatesQueueing(t *testing.T) {
	var ownership taskOwnership
	var queued atomic.Int32
	var acquired atomic.Int32
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			if ownership.enqueue() {
				queued.Add(1)
			}
		})
	}
	group.Wait()
	for range 32 {
		group.Go(func() {
			if ownership.acquire() {
				acquired.Add(1)
			}
		})
	}
	group.Wait()
	if queued.Load() != 1 || acquired.Load() != 1 {
		t.Fatalf("nonexclusive dispatch: queued=%d acquired=%d", queued.Load(), acquired.Load())
	}
	if ownership.enqueue() {
		t.Fatal("running continuation requeued before handoff")
	}
	ownership.relinquish()
	if !ownership.enqueue() || !ownership.acquire() {
		t.Fatal("returned continuation cannot move to another worker")
	}
	ownership.relinquish()
	ownership.terminate()
	if ownership.enqueue() || ownership.acquire() {
		t.Fatal("terminal continuation was dispatched")
	}
}

func TestTaskWaitRetainsEarlyCompletionAndRejectsPreviousTicket(t *testing.T) {
	var ownership taskOwnership
	ownership.enqueue()
	ownership.acquire()
	first := ownership.beginWait()
	if !ownership.notify(first) || ownership.resume(first) || ownership.enqueue() {
		t.Fatal("completion took ownership during parking")
	}
	ownership.relinquish()
	if !ownership.notified(first) || !ownership.resume(first) {
		t.Fatal("completion preceding handoff was lost")
	}
	ownership.enqueue()
	ownership.acquire()
	second := ownership.beginWait()
	ownership.relinquish()
	if ownership.notify(first) || ownership.resume(first) || ownership.notified(second) {
		t.Fatal("late completion affected a subsequent wait")
	}
	if !ownership.notify(second) || !ownership.notified(second) || !ownership.resume(second) {
		t.Fatal("current completion did not resume its wait")
	}
	ownership.terminate()
	if ownership.notify(second) || ownership.resume(second) {
		t.Fatal("completion revived terminated task")
	}
}

func TestTaskWaitNotificationRacesWithHandoffWithoutLosingOwnership(t *testing.T) {
	for range 1000 {
		var ownership taskOwnership
		ownership.enqueue()
		ownership.acquire()
		ticket := ownership.beginWait()
		var group sync.WaitGroup
		group.Go(func() { ownership.notify(ticket) })
		group.Go(func() { ownership.relinquish() })
		group.Wait()
		if !ownership.notified(ticket) || !ownership.resume(ticket) || ownership.resume(ticket) {
			t.Fatal("completion lost or resumed twice across handoff")
		}
	}
}

func TestRunnableQueueDoesNotRepeatTheSameContinuation(t *testing.T) {
	machine := &executionMachine{}
	task := &executionTask{}
	machine.pushRunnable(task, task)
	machine.pushRunnableFront(task)
	if machine.runnableCount() != 1 || machine.popRunnable() != task || machine.popRunnable() != nil {
		t.Fatal("duplicate ready insertion duplicated a continuation")
	}
}
