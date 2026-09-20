package runtime

import (
	"sync"
	"testing"
)

func TestConcurrentModuleTypeResolutionAndSharedCells(t *testing.T) {
	module := &moduleInstance{}
	typ := runtimeTypeFromText("struct{A:Int,B:Array<2, Int>}")
	cell := newSlot(typ, module, false)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 100 {
				value := cell.load()
				if !value.Type.Equal(typ) {
					t.Errorf("incorrect cell type: %v", value.Type)
				}
				resolved := module.resolvedRuntimeType(typ.String())
				if !resolved.Equal(typ) {
					t.Errorf("incorrect resolved type: %v", resolved)
				}
				if err := cell.store(module.zeroValue(typ)); err != nil {
					t.Error(err)
				}
				fields, ok := materializeStructValue(cell.load().Data)
				if !ok || len(fields) != 2 {
					t.Errorf("invalid struct fields: %v", fields)
				}
			}
		})
	}
	group.Wait()
}

func TestConcurrentReflectionTypePublicationKeepsCanonicalValue(t *testing.T) {
	machine := &vm{}
	info := TypeInfo{Key: "Int", Kind: "int", Type: "Int"}
	values := make(chan vmValue, 4)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 100 {
				value := reflectTypeValueFromVM(machine, info)
				resolved, err := reflectResolvedTypeInfo(intrinsicContext{vm: machine}, value)
				if err != nil || resolved.Key != info.Key {
					t.Errorf("resolved reflection type = %v, %v", resolved, err)
					return
				}
			}
			values <- reflectTypeValueFromVM(machine, info)
		})
	}
	group.Wait()
	close(values)
	var canonical *vmStruct
	for value := range values {
		payload, ok := value.Data.(vmValue)
		if !ok {
			t.Fatalf("reflection type value = %T", value.Data)
		}
		structure, ok := payload.Data.(*vmStruct)
		if !ok {
			t.Fatalf("reflection type payload = %T", value.Data)
		}
		if canonical == nil {
			canonical = structure
		}
		if canonical != structure {
			t.Fatal("concurrent publication produced distinct type values")
		}
	}
	retained := machine.reflectTypeValues.snapshot()
	machine.reflectTypeValues.clear()
	if _, ok := machine.reflectTypeValues.load(info.Key); ok {
		t.Fatal("cache clear retained an entry")
	}
	if retained[info.Key].Data.(vmValue).Data != canonical {
		t.Fatal("cache clear changed the owned snapshot")
	}
}
