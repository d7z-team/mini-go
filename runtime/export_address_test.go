package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestExportAddressPreservesInitializedSlotAcrossPatch(t *testing.T) {
	root, dependency := exportAddressArtifacts()
	program := patchMultiModuleProgram(t, root, dependency, "address-old")
	instance, err := program.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestInstance(t, instance)
	result, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 1 {
		t.Fatalf("initial result: %v", result.Values)
	}
	saved := instance.vm.rootModule().state.globals["global.saved"].load()
	pointer, err := pointerValue(saved)
	if err != nil {
		t.Fatal(err)
	}
	badRoot, badDependency := exportAddressArtifacts()
	badDependency.Globals = append(badDependency.Globals, ir.Global{ID: "global.extra", Type: testType("Int64")})
	bad := patchMultiModuleProgram(t, badRoot, badDependency, "address-incompatible")
	if _, err := instance.PreparePatch(context.Background(), bad); patchErrorCode(err) != "global_shape_changed" {
		t.Fatalf("incompatible patch: %v", err)
	}
	root, dependency = exportAddressArtifacts()
	dependency.Constants[0].Value = []byte("2")
	next := patchMultiModuleProgram(t, root, dependency, "address-new")
	plan, err := instance.PreparePatch(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if pointer.slot.module.revision.generation != 2 {
		t.Fatal("pointer retained the old slot owner")
	}
	value, err := pointer.loadValue()
	number, ok := value.signedValue()
	if err != nil || !ok || number != 1 {
		t.Fatalf("patch reran init or broke pointer: %+v, %v", value, err)
	}
	if err := storePointer(saved, newVMValue("Int64", int64(9))); err != nil {
		t.Fatal(err)
	}
	result, err = instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 9 {
		t.Fatalf("patched result: %v", result.Values)
	}
	fresh, err := pointerValue(instance.vm.rootModule().state.globals["global.saved"].load())
	if err != nil || fresh.Identity != pointer.Identity {
		t.Fatalf("pointer identity changed: %v", err)
	}
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil || len(retained) != 1 || retained[0].Generation != 2 {
		t.Fatalf("retained revisions: %v, %v", retained, err)
	}
}

func TestExportAddressInitializationFailure(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "panic"
		if cancel {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			root, dependency := exportAddressArtifacts()
			if cancel {
				dependency.Functions[0].Instructions = []ir.Instruction{
					{Op: string(ir.OpLabel), Payload: testPayload(ir.LabelPayload{Label: "loop"})},
					{Op: string(ir.OpJump), Payload: testPayload(ir.JumpPayload{Label: "loop"})},
				}
			} else {
				dependency.Functions[0].Instructions = []ir.Instruction{
					{Op: string(ir.OpConst), Payload: testPayload(ir.ConstPayload{Constant: "const.initial"})},
					{Op: string(ir.OpPanic)},
				}
			}
			instance, err := patchMultiModuleProgram(t, root, dependency, name).Instantiate(context.Background(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestInstance(t, instance)
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				if state, _, err := execution.PollSteps(8); err != nil || state != ExecutionRunning {
					t.Fatalf("init poll: %s %v", state, err)
				}
				execution.Cancel()
			}
			_, err = execution.Wait(context.Background())
			if err == nil || cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("init failure: %v", err)
			}
			if saved := instance.vm.rootModule().state.globals["global.saved"].load(); saved.Data != nil {
				t.Fatalf("delivered pointer before successful init: %+v", saved)
			}
		})
	}
}

func TestExportAddressRequiresGlobalExport(t *testing.T) {
	root, dependency := exportAddressArtifacts()
	dependency.Exports[0] = ir.Export{Name: "Item", Kind: "const", ID: "const.initial", Type: testType("Int64")}
	modules := map[string]*executable{}
	for _, artifact := range []ir.Artifact{root, dependency} {
		attachRuntimeTestTypeNodes(&artifact)
		code, err := newLoader().load(artifact)
		if err != nil {
			t.Fatal(err)
		}
		modules[artifact.Module.Path] = code
	}
	if _, err := prepareModuleReferences(modules); err == nil || !strings.Contains(err.Error(), "not an exported variable") {
		t.Fatalf("link: %v", err)
	}
}

func TestModuleExportsObserveInitializationState(t *testing.T) {
	for _, test := range []struct {
		name                  string
		address, initializing bool
		want                  int64
	}{
		{"read_initializes", false, false, 1},
		{"address_initializes", true, false, 1},
		{"read_during_init", false, true, 0},
		{"address_during_init", true, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, dependency := exportAddressArtifacts()
			if !test.address {
				root.Functions[0].Instructions = []ir.Instruction{
					{Op: string(ir.OpLoadExport), Payload: testPayload(ir.ExportPayload{ModulePath: dependency.Module.Path, Export: "Item"})},
					{Op: string(ir.OpLoadField), Payload: testPayload(ir.FieldPayload{Field: "N"})},
					{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{ResultCount: 1})},
				}
			}
			instance, err := patchMultiModuleProgram(t, root, dependency, test.name).Instantiate(context.Background(), InstanceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cleanupTestInstance(t, instance)
			module, ok := instance.vm.moduleRegistry().module(dependency.Module.Path)
			if !ok {
				t.Fatal("dependency is missing from the instance")
			}
			if test.initializing {
				module.state.beginInitialization()
			}
			execution, err := instance.Start("run")
			if err != nil {
				t.Fatal(err)
			}
			if test.initializing {
				module.state.initTask = instance.vm.machine.runnableTasks()[0]
			}
			result, err := execution.Wait(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n, ok := result.Values[0].Int64(); !ok || n != test.want {
				t.Fatalf("initial result = %v, want %d", result.Values, test.want)
			}
			if test.initializing {
				module.state.finishInitialization(nil)
			}
			pointer := newPathPointerValue("Int64", "item.N", module.state.globals["global.item"], []ir.AddressPathSegment{{Kind: "field", Field: "N"}}, nil)
			if err := storePointer(pointer, newVMValue("Int64", int64(7))); err != nil {
				t.Fatal(err)
			}
			result, err = instance.Call(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			if n, ok := result.Values[0].Int64(); !ok || n != 7 {
				t.Fatalf("ready module reran init: %v", result.Values)
			}
		})
	}
}
