package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/d7z-team/mini-go/ffi"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestInstantiateCompletesRootInitialization(t *testing.T) {
	artifact := ir.NewArtifact("instance/init", "main")
	artifact.Constants = []ir.Constant{{ID: "const.true", Type: testType("Bool"), Value: json.RawMessage(`true`)}}
	artifact.Globals = []ir.Global{{ID: "global.ready", Type: testType("Bool")}}
	artifact.Functions = []ir.Function{
		{ID: moduleInitFunctionID, Signature: testSignature("function() Void"), Code: testSlotCode([]string{"Bool"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.true"}},
			{Op: ir.OpStoreGlobal, Payload: ir.GlobalPayload{Global: "global.ready"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}, {nil, nil}})},
		{ID: "fn.entry", Signature: testSignature("function() Bool"), Code: testSlotCode([]string{"Bool"}, []ir.Instruction{
			{Op: ir.OpLoadGlobal, Payload: ir.GlobalPayload{Global: "global.ready"}},
			{Op: ir.OpReturn, Payload: ir.ReturnPayload{ResultCount: 1}},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}})},
	}
	instance, err := patchTestProgram(t, artifact, "instance-init").Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	if instance.vm.rootModule().state.initState != moduleReady {
		t.Fatalf("root init state = %d", instance.vm.rootModule().state.initState)
	}
	result, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Bool(); !ok || !value {
		t.Fatalf("initialized value = %#v", result.Values)
	}
}

func TestInstantiateRequiresProgramHostCapabilities(t *testing.T) {
	program := patchTestProgram(t, patchGlobalArtifact(1), "capability-required")
	program.code.image.Capabilities = []string{"console"}
	if _, err := program.Instantiate(context.Background(), InstanceOptions{}); err == nil {
		t.Fatal("Instantiate accepted a program without its host capability")
	}
	plain := &plainInstanceBridge{}
	if _, err := program.Instantiate(context.Background(), InstanceOptions{FFI: plain}); err == nil {
		t.Fatal("Instantiate treated an ordinary Bridge as a capability provider")
	}
	if plain.opened != 0 {
		t.Fatal("Instantiate opened FFI before validating capabilities")
	}
	invalid := &instanceLifecycleBridge{capabilities: []string{"console", "console"}}
	if _, err := program.Instantiate(context.Background(), InstanceOptions{FFI: invalid}); err == nil {
		t.Fatal("Instantiate accepted duplicate bridge capabilities")
	}
	if invalid.opened != 0 {
		t.Fatal("Instantiate opened FFI after rejecting bridge capabilities")
	}
	bridge := &instanceLifecycleBridge{capabilities: []string{"console"}}
	instance, err := program.Instantiate(context.Background(), InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
}

type plainInstanceBridge struct{ opened int }

func (b *plainInstanceBridge) Open(context.Context) (ffi.Session, error) {
	b.opened++
	return plainInstanceSession{}, nil
}

type plainInstanceSession struct{}

func (plainInstanceSession) Start(context.Context, ffi.Request, ffi.Completion) (ffi.Call, error) {
	return ffi.CancelFunc(func() {}), nil
}

func (plainInstanceSession) Shutdown(context.Context) error { return nil }

func TestInstantiateFailureClosesFFISession(t *testing.T) {
	artifact := ir.NewArtifact("instance/init-panic", "main")
	artifact.Constants = []ir.Constant{{ID: "const.failure", Type: testType("String"), Value: json.RawMessage(`"init failed"`)}}
	artifact.Functions = []ir.Function{
		{ID: moduleInitFunctionID, Signature: testSignature("function() Void"), Code: testSlotCode([]string{"String"}, []ir.Instruction{
			{Op: ir.OpConst, Payload: ir.ConstPayload{Constant: "const.failure"}},
			{Op: ir.OpPanic},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}})},
		{ID: "fn.entry", Signature: testSignature("function() Void"), Code: testSlotCode([]string{}, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}})},
	}
	bridge := &instanceLifecycleBridge{}
	if _, err := patchTestProgram(t, artifact, "instance-init-panic").Instantiate(context.Background(), InstanceOptions{FFI: bridge}); err == nil {
		t.Fatal("Instantiate accepted a panicking root init")
	}
	if bridge.opened != 1 || bridge.shutdown != 1 {
		t.Fatalf("FFI lifecycle after init failure: opened=%d shutdown=%d", bridge.opened, bridge.shutdown)
	}
}

type instanceLifecycleBridge struct {
	capabilities []string
	opened       int
	shutdown     int
}

func (b *instanceLifecycleBridge) HostCapabilities() []string {
	return append([]string(nil), b.capabilities...)
}

func (b *instanceLifecycleBridge) Open(context.Context) (ffi.Session, error) {
	b.opened++
	return instanceLifecycleSession{owner: b}, nil
}

type instanceLifecycleSession struct{ owner *instanceLifecycleBridge }

func (instanceLifecycleSession) Start(context.Context, ffi.Request, ffi.Completion) (ffi.Call, error) {
	return ffi.CancelFunc(func() {}), nil
}

func (s instanceLifecycleSession) Shutdown(context.Context) error {
	s.owner.shutdown++
	return nil
}

func TestNamedEntryDoesNotBecomeDefault(t *testing.T) {
	program := patchTestProgram(t, patchGlobalArtifact(1), "named-entry")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)

	if _, err := instance.CallEntry(context.Background()); err == nil || err.Error() != "program has no default entry" {
		t.Fatalf("CallEntry error = %v", err)
	}
	result, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 1 {
		t.Fatalf("named entry result = %#v", result.Values)
	}

	plan, err := instance.PreparePatch(context.Background(), patchTestProgram(t, patchGlobalArtifact(2), "named-entry-patched"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.CallEntry(context.Background()); err == nil || err.Error() != "program has no default entry" {
		t.Fatalf("patched CallEntry error = %v", err)
	}
	result, err = instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 3 {
		t.Fatalf("patched named entry result = %#v", result.Values)
	}
}

func TestInstanceOwnsOneFFISessionAcrossPatches(t *testing.T) {
	bridge := &instanceLifecycleBridge{}
	program := patchTestProgram(t, patchGlobalArtifact(1), "ffi-session")
	first, err := program.Instantiate(context.Background(), InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	second, err := program.Instantiate(context.Background(), InstanceOptions{FFI: bridge})
	if err != nil {
		t.Fatal(err)
	}
	if bridge.opened != 2 {
		t.Fatalf("opened sessions = %d", bridge.opened)
	}
	plan, err := first.PreparePatch(context.Background(), patchTestProgram(t, patchGlobalArtifact(2), "ffi-session-patched"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if bridge.opened != 2 {
		t.Fatalf("patch opened another session: %d", bridge.opened)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if bridge.shutdown != 1 {
		t.Fatalf("shutdown sessions after first close = %d", bridge.shutdown)
	}
	if _, err := second.Call(context.Background(), "run"); err != nil {
		t.Fatalf("second instance after first close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if bridge.shutdown != 2 {
		t.Fatalf("shutdown sessions = %d", bridge.shutdown)
	}
}
