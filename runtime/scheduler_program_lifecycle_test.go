package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestProgramMainReturnStopsSpawnedTasks(t *testing.T) {
	artifact := lifecycleArtifact(delayedLifecycleChild(
		ir.Instruction{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.failure"})},
		ir.Instruction{Op: string(ir.OpPanic)},
	))
	artifact.Constants = []ir.Constant{{ID: "const.failure", Type: testType("String"), Value: json.RawMessage(`"too late"`)}}
	program := patchTestProgram(t, artifact, "program-main-return")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	execution, err := instance.StartMain()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatalf("main returned with delayed child panic: %v", err)
	}
	if scope, err := execution.WaitScope(context.Background()); err != nil || !scope.Done || scope.RootState != ExecutionCompleted {
		t.Fatalf("main scope = %#v, %v", scope, err)
	}
	select {
	case <-instance.Done():
	default:
		t.Fatal("program instance did not become terminal after main returned")
	}
	if err := instance.Err(); err != nil {
		t.Fatalf("program terminal error = %v", err)
	}
	if instance.State() != InstanceClosed {
		t.Fatalf("program instance state = %s", instance.State())
	}
}

func TestCancelProgramMainClosesOtherScopes(t *testing.T) {
	artifact := lifecycleArtifact([]ir.Instruction{
		{Op: string(ir.OpZero), Payload: testPayload(ir.TypePayload{Type: testType("Waitable<Int>")})},
		{Op: string(ir.OpWaitableRecv)},
		{Op: string(ir.OpPop)},
		{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})},
	})
	instance, err := patchTestProgram(t, artifact, "program-main-cancel").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	background, err := instance.Start("run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := background.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	main, err := instance.StartMain()
	if err != nil {
		t.Fatal(err)
	}
	main.Cancel()
	if main.State() != ExecutionCanceled || !errors.Is(main.Err(), context.Canceled) {
		t.Fatalf("canceled main = %s, %v", main.State(), main.Err())
	}
	if scope, err := background.WaitScope(context.Background()); !errors.Is(err, context.Canceled) || !scope.Done {
		t.Fatalf("background scope after main cancel = %#v, %v", scope, err)
	}
	select {
	case <-instance.Done():
	default:
		t.Fatal("canceled main did not terminate the instance")
	}
	if instance.vm.machine != nil {
		t.Fatal("canceled main retained scheduler state")
	}
}

func TestProgramChildPanicBeforeMainReturnFails(t *testing.T) {
	artifact := lifecycleArtifact([]ir.Instruction{
		{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.failure"})},
		{Op: string(ir.OpPanic)},
	})
	artifact.Constants = []ir.Constant{{ID: "const.failure", Type: testType("String"), Value: json.RawMessage(`"early"`)}}
	instance, err := patchTestProgram(t, artifact, "program-child-panic").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if _, err := instance.CallMain(context.Background()); err == nil {
		t.Fatal("main ignored a child panic observed before return")
	}
	if !errors.Is(instance.unavailableError(), ErrInstanceFaulted) || instance.Err() == nil {
		t.Fatalf("instance after child panic: state=%d err=%v", instance.lifecycleState(), instance.Err())
	}
}
