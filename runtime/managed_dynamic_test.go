package runtime

import "testing"

func TestManagedDynamicCellPreservesDeclaredAndConcreteTypes(t *testing.T) {
	module := newManagedValueTestModule()
	for _, declared := range []string{"Any", "interface{}"} {
		t.Run(declared, func(t *testing.T) {
			typ := coerceRuntimeType(declared)
			left, err := module.newManagedCell(typ, newVMValue(declared, newVMValue("Ptr<Int>", nil)))
			if err != nil {
				t.Fatal(err)
			}
			right, err := module.newManagedCell(typ, newVMValue(declared, newVMValue("Ptr<String>", nil)))
			if err != nil {
				t.Fatal(err)
			}
			leftValue, err := left.read()
			if err != nil {
				t.Fatal(err)
			}
			rightValue, err := right.read()
			if err != nil {
				t.Fatal(err)
			}
			if !leftValue.Type.Equal(typ) || !rightValue.Type.Equal(typ) {
				t.Fatalf("dynamic cell lost declared type: %s / %s", leftValue.Type, rightValue.Type)
			}
			if equal, err := module.equalValues(leftValue, rightValue); err != nil || equal {
				t.Fatalf("different concrete pointer types compared equal: %v, %v", equal, err)
			}
			if equal, err := module.equalValues(leftValue, newVMValue(declared, nil)); err != nil || equal {
				t.Fatalf("typed nil compared equal to nil interface: %v, %v", equal, err)
			}
			if err := right.write(newVMValue(declared, newVMValue("Ptr<Int>", nil))); err != nil {
				t.Fatal(err)
			}
			rightValue, err = right.read()
			if err != nil {
				t.Fatal(err)
			}
			if equal, err := module.equalValues(leftValue, rightValue); err != nil || !equal {
				t.Fatalf("identical concrete nil pointers differ: %v, %v", equal, err)
			}
		})
	}
}

func TestManagedTypedNilDoesNotAcquireStorageIdentity(t *testing.T) {
	module := newManagedValueTestModule()
	for _, value := range []vmValue{
		newVMValue("Ptr<Int>", (*vmPointer)(nil)),
		newVMValue("Map<String,Int>", (*vmMap)(nil)),
		newVMValue("Waitable<Int>", (*waitableResource)(nil)),
	} {
		t.Run(value.Type.String(), func(t *testing.T) {
			published, err := module.importDetachedValue(value, map[uintptr]guestRef{})
			if err != nil || published.ref.valid() || !isNilRuntimeValue(published) {
				t.Fatalf("typed nil publication = %+v, %v", published, err)
			}
			cell, err := module.newManagedCell(coerceRuntimeType("Any"), newVMValue("Any", value))
			if err != nil {
				t.Fatal(err)
			}
			dynamic, err := cell.read()
			if err != nil {
				t.Fatal(err)
			}
			concrete, ok := dynamic.payload().(vmValue)
			if !ok || concrete.Type.String() != value.Type.String() || !isNilRuntimeValue(concrete) {
				t.Fatalf("dynamic typed nil = %+v", dynamic)
			}
		})
	}
}

func TestInlineNilInterfaceHasNoDynamicType(t *testing.T) {
	module := newManagedValueTestModule()
	typ := predeclaredRuntimeTypes["Any"]
	layout, err := module.vm.valueLayouts.layoutFor(typ)
	if err != nil {
		t.Fatal(err)
	}
	var bytes [24]byte
	if err := module.writeInlineValue(bytes[:], newVMValue(typ, nil), map[uintptr]guestRef{}); err != nil {
		t.Fatal(err)
	}
	value := readInlineAggregateOwned(module, typ, layout, bytes[:], nil)
	if !isNilDynamicInterface(module, value) {
		t.Fatalf("nil interface acquired a dynamic value: %+v", value)
	}
}
