package runtime

import "testing"

func TestCompareLengthValueUsesRawSignedAndUnsignedOperands(t *testing.T) {
	for _, test := range []struct {
		name     string
		operator preparedOperator
		right    vmValue
		want     bool
	}{
		{name: "signed equal", operator: operatorEqual, right: newSignedVMValue(coerceRuntimeType("Int"), 4), want: true},
		{name: "signed less", operator: operatorLess, right: newSignedVMValue(coerceRuntimeType("Int"), 5), want: true},
		{name: "unsigned greater", operator: operatorGreater, right: newUnsignedVMValue(coerceRuntimeType("Uint"), 3), want: true},
		{name: "unsigned equal", operator: operatorEqual, right: newUnsignedVMValue(coerceRuntimeType("Uint"), 4), want: true},
		{name: "unsigned maximum", operator: operatorLess, right: newUnsignedVMValue(coerceRuntimeType("Uint64"), ^uint64(0)), want: true},
		{name: "negative threshold", operator: operatorGreater, right: newSignedVMValue(coerceRuntimeType("Int8"), -1), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := compareLengthValue(test.operator, 4, test.right)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("compareLengthValue = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCompareLengthValueRejectsNonIntegerOperand(t *testing.T) {
	_, err := compareLengthValue(operatorEqual, 4, newVMValue("String", "4"))
	if err == nil {
		t.Fatal("accepted a non-integer comparison operand")
	}
}
