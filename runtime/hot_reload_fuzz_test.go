package runtime

import (
	"context"
	"maps"
	"sort"
	"strings"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func FuzzPatchTransaction(f *testing.F) {
	for _, seed := range []struct {
		oldDelta, newDelta    int64
		shape, apply, symbols uint8
	}{{1, 2, 0, 0, 0}, {-3, 5, 1, 0, 1}, {1, 2, 2, 0, 2}, {1, 2, 3, 0, 3}, {1, 2, 4, 0, 0}, {1, 2, 5, 0, 1}, {1, 2, 6, 0, 2}, {1, 2, 7, 0, 3}, {1, 2, 0, 1, 0}, {1, 2, 0, 2, 1}, {1, 2, 0, 3, 2}} {
		f.Add(seed.oldDelta, seed.newDelta, seed.shape, seed.apply, seed.symbols)
	}
	f.Fuzz(func(t *testing.T, oldInput, newInput int64, shapeInput, applyInput, symbolInput uint8) {
		oldDelta, newDelta := oldInput%1000, newInput%1000
		if newDelta == oldDelta {
			newDelta++
		}
		shapeMode, applyMode := shapeInput%8, applyInput%3
		base := patchTestProgram(t, patchGlobalArtifact(oldDelta), "fuzz-base")
		if symbolInput&1 != 0 {
			base = base.WithoutSymbols()
		}
		targetArtifact := patchGlobalArtifact(newDelta)
		compatible := shapeMode < 2
		switch shapeMode {
		case 1:
			targetArtifact.Functions = append(targetArtifact.Functions, ir.Function{
				ID: "fn.added", Signature: testSignature("function() Void"),
				Instructions: []ir.Instruction{{Op: string(ir.OpReturn), Payload: testPayload(ir.ReturnPayload{})}},
			})
		case 2:
			targetArtifact.Globals[0].Type = testType("String")
		case 3:
			targetArtifact.Exports = nil
		case 5:
			targetArtifact.Module.Package = "changed"
		case 6:
			targetArtifact.Globals[0].ID = "global.changed"
			for index := range targetArtifact.Functions[0].Instructions {
				instruction := &targetArtifact.Functions[0].Instructions[index]
				if instruction.Op == string(ir.OpLoadGlobal) || instruction.Op == string(ir.OpStoreGlobal) {
					instruction.Payload = testPayload(ir.GlobalPayload{Global: "global.changed"})
				}
			}
		case 7:
			targetArtifact.Globals = append(targetArtifact.Globals, ir.Global{ID: "global.added", Type: testType("Int64")})
		}
		target := patchTestProgram(t, targetArtifact, "fuzz-target")
		if symbolInput&2 != 0 {
			target = target.WithoutSymbols()
		}
		target.code.image.Target.Tags = append([]string(nil), base.code.image.Target.Tags...)
		if shapeMode == 4 {
			target.code.image.Root = "fuzz/other"
		}

		instance, err := base.Instantiate(context.Background(), InstanceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cleanupTestInstance(t, instance)
		first, err := instance.Call(context.Background(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := first.Values[0].Int64(); !ok || value != oldDelta {
			t.Fatalf("initial value = %#v, want %d", first.Values, oldDelta)
		}
		cell := instance.vm.rootModule().state.globals["global.total"]
		baseRevision := instance.Revision()
		baseRevisionState := instance.vm.revision.Load()
		baseEntries := maps.Clone(baseRevisionState.entries)
		plan, err := instance.PreparePatch(context.Background(), target)
		if !compatible {
			if err == nil {
				t.Fatalf("shape mode %d unexpectedly prepared", shapeMode)
			}
			requireUnchangedPatchInstance(t, instance, baseRevision, baseRevisionState, cell, baseEntries, oldDelta+oldDelta)
			return
		}
		if err != nil {
			t.Fatalf("compatible shape failed to prepare: %v", err)
		}

		switch applyMode {
		case 0:
			if _, err := instance.ApplyPatch(plan); err != nil {
				t.Fatal(err)
			}
			if instance.Revision().SymbolsHash != target.SymbolsHash() {
				t.Fatalf("patched symbols = %q, want %q", instance.Revision().SymbolsHash, target.SymbolsHash())
			}
			if instance.vm.rootModule().state.globals["global.total"] != cell {
				t.Fatal("successful patch replaced global slot")
			}
			result, err := instance.Call(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			if value, ok := result.Values[0].Int64(); !ok || value != oldDelta+newDelta {
				t.Fatalf("patched value = %#v, want %d", result.Values, oldDelta+newDelta)
			}
		case 1:
			instance.vm.rootModule().state.beginInitialization()
			_, err := instance.ApplyPatch(plan)
			state := instance.vm.rootModule().state
			state.finishInitialization(nil)
			state.initState = moduleUninitialized
			if patchErrorCode(err) != "module_initializing" {
				t.Fatalf("initializing patch error = %v", err)
			}
			requireUnchangedPatchInstance(t, instance, baseRevision, baseRevisionState, cell, baseEntries, oldDelta+oldDelta)
		case 2:
			if err := plan.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := instance.ApplyPatch(plan); patchErrorCode(err) != "invalid_plan" {
				t.Fatalf("closed patch error = %v", err)
			}
			requireUnchangedPatchInstance(t, instance, baseRevision, baseRevisionState, cell, baseEntries, oldDelta+oldDelta)
		}
	})
}

func FuzzPatchCapabilityTransaction(f *testing.F) {
	f.Add("console", "filesystem", false)
	f.Add("console", "console", true)
	f.Add("", "entropy", true)
	f.Fuzz(func(t *testing.T, baseName, targetName string, installTarget bool) {
		if len(baseName) > 128 || len(targetName) > 128 {
			t.Skip()
		}
		baseName = strings.TrimSpace(baseName)
		targetName = strings.TrimSpace(targetName)
		base := patchTestProgram(t, patchGlobalArtifact(1), "capability-fuzz-base")
		if baseName != "" {
			base.code.image.Capabilities = []string{baseName}
		}
		target := patchTestProgram(t, patchGlobalArtifact(2), "capability-fuzz-target")
		capabilities := []string{}
		if baseName != "" {
			capabilities = append(capabilities, baseName)
		}
		if targetName != "" && targetName != baseName {
			capabilities = append(capabilities, targetName)
		}
		sort.Strings(capabilities)
		target.code.image.Capabilities = capabilities
		installed := []string{}
		if baseName != "" {
			installed = append(installed, baseName)
		}
		if installTarget && targetName != "" && targetName != baseName {
			installed = append(installed, targetName)
		}
		instance, err := base.Instantiate(context.Background(), InstanceOptions{FFI: &instanceLifecycleBridge{capabilities: installed}})
		if err != nil {
			t.Fatal(err)
		}
		cleanupTestInstance(t, instance)
		cell := instance.vm.rootModule().state.globals["global.total"]
		before := instance.Revision()
		plan, err := instance.PreparePatch(context.Background(), target)
		available := targetName == "" || targetName == baseName || installTarget
		if !available {
			if patchErrorCode(err) != "capability_unavailable" {
				t.Fatalf("capability error = %v", err)
			}
			if instance.Revision() != before || instance.vm.rootModule().state.globals["global.total"] != cell {
				t.Fatal("failed capability patch changed revision or global slot")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := instance.ApplyPatch(plan); err != nil {
			t.Fatal(err)
		}
		if instance.Revision().Generation != before.Generation+1 || instance.vm.rootModule().state.globals["global.total"] != cell {
			t.Fatal("successful capability patch did not preserve state transaction")
		}
	})
}

func requireUnchangedPatchInstance(t *testing.T, instance *Instance, revision RevisionInfo, revisionState *instanceRevision, cell *slot, entries map[string]string, want int64) {
	t.Helper()
	if instance.Revision() != revision || instance.vm.rootModule().state.globals["global.total"] != cell ||
		instance.vm.revision.Load() != revisionState || !maps.Equal(instance.vm.revision.Load().entries, entries) {
		t.Fatal("failed patch changed revision, entry, or global slot")
	}
	result, err := instance.Call(context.Background(), "run")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != want {
		t.Fatalf("value after failed patch = %#v, want %d", result.Values, want)
	}
}
