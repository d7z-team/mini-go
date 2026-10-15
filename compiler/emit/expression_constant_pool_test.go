package emit

import (
	"testing"

	"github.com/d7z-team/mini-go/compiler/constant"
	"github.com/d7z-team/mini-go/compiler/hir"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestAnonymousConstantPoolPreservesWireIdentity(t *testing.T) {
	float64Type := testHIRType("Float64")
	complex64Type := testHIRType("Complex64")
	complex128Type := testHIRType("Complex128")
	namedFloatNode := types.TypeNode{
		ID:         "decl.example.Measurement",
		Kind:       types.Named,
		Identity:   types.TypeKey{ModulePath: "example", DeclID: "Measurement"},
		Underlying: float64Type,
	}
	namedFloatType := types.Ref(namedFloatNode)

	numeric := func(text, typ string, untyped bool) *constant.Value {
		value, ok := constant.Numeric(text, typ, untyped)
		if !ok {
			t.Fatalf("invalid numeric fixture %q", text)
		}
		return value.Ref()
	}
	complexValue := func(typ string) *constant.Value {
		value, ok := constant.Complex(*constant.Scalar("1/2"), *constant.Scalar("-1/4"), typ, false)
		if !ok {
			t.Fatalf("invalid complex fixture %s", typ)
		}
		return value.Ref()
	}
	literal := func(typ types.TypeRef, value *constant.Value, untyped bool) hir.Expression {
		return hir.Expression{Kind: hir.ExprLiteral, Type: typ, Value: value, Untyped: untyped}
	}

	values := []struct {
		local string
		expr  hir.Expression
		typ   types.TypeRef
	}{
		{"float.first", literal(float64Type, numeric("1/2", "Float64", false), false), float64Type},
		{"float.second", literal(float64Type, numeric("1/2", "Float64", false), false), float64Type},
		{"float.named", literal(namedFloatType, numeric("1/2", "Measurement", false), false), namedFloatType},
		{"float.negzero.first", literal(float64Type, constant.Scalar("-0"), false), float64Type},
		{"float.negzero.second", literal(float64Type, constant.Scalar("-0"), false), float64Type},
		{"float.poszero", literal(float64Type, constant.Scalar("0"), false), float64Type},
		{"complex128.first", literal(complex128Type, complexValue("Complex128"), false), complex128Type},
		{"complex128.second", literal(complex128Type, complexValue("Complex128"), false), complex128Type},
		{"complex64", literal(complex64Type, complexValue("Complex64"), false), complex64Type},
	}
	program := hir.Program{
		ModulePath: "example/constants",
		Package:    "constants",
		TypeTable:  types.TypeTable{Nodes: []types.TypeNode{namedFloatNode}},
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Void"),
		}},
	}
	function := &program.Functions[0]
	for _, value := range values {
		function.Locals = append(function.Locals, hir.Local{ID: value.local, Type: value.typ})
		function.Body = append(function.Body, hir.Statement{Kind: hir.StmtStoreLocal, Local: value.local, Expr: value.expr})
	}

	artifact, err := lowerTestProgram(t, program)
	if err != nil {
		t.Fatal(err)
	}
	code := artifact.Functions[0].Code
	constantByLocal := make(map[string]uint32, len(values))
	for _, instruction := range code.Instructions {
		if instruction.Op != ir.OpStoreLocal {
			continue
		}
		payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		local := payload.(ir.LocalPayload).Local
		inputs := code.Operands[instruction.Operands].Inputs
		if len(inputs) != 1 || inputs[0].Kind != ir.OperandConstant {
			t.Fatalf("literal store %s lost its constant operand: %#v", local, inputs)
		}
		constantByLocal[local] = inputs[0].Index
	}

	for _, value := range values {
		if _, ok := constantByLocal[value.local]; !ok {
			t.Fatalf("missing literal store for %s", value.local)
		}
	}
	if got := constantByLocal["float.first"]; got != constantByLocal["float.second"] {
		t.Fatalf("equal typed literals do not share a canonical constant: %d and %d", got, constantByLocal["float.second"])
	}
	if constantByLocal["float.first"] == constantByLocal["float.named"] {
		t.Fatal("named and builtin float literals share type identity")
	}
	if got := constantByLocal["float.negzero.first"]; got != constantByLocal["float.negzero.second"] {
		t.Fatalf("equal negative zero literals do not share a canonical constant: %d and %d", got, constantByLocal["float.negzero.second"])
	}
	if constantByLocal["float.negzero.first"] == constantByLocal["float.poszero"] {
		t.Fatal("negative zero and positive zero share wire identity")
	}
	if got := constantByLocal["complex128.first"]; got != constantByLocal["complex128.second"] {
		t.Fatalf("equal complex literals do not share a canonical constant: %d and %d", got, constantByLocal["complex128.second"])
	}
	if constantByLocal["complex128.first"] == constantByLocal["complex64"] {
		t.Fatal("complex values with different declared types share wire identity")
	}

	if got := len(artifact.Constants); got != 6 {
		t.Fatalf("unexpected canonical constant pool size: got %d, want 6", got)
	}
	if got := string(artifact.Constants[constantByLocal["float.negzero.first"]].Value); got != "-0" {
		t.Fatalf("negative zero encoding = %s", got)
	}
	if got := string(artifact.Constants[constantByLocal["complex128.first"]].Value); got != `{"real":0.5,"imag":-0.25}` {
		t.Fatalf("complex encoding = %s", got)
	}

	// Untyped constants are compile-time operands and are rejected by the
	// execution-image validator. Exercise their pool identity directly at the
	// expression-emission boundary instead of smuggling one into executable
	// code.
	compileOnly := ir.NewArtifact("example/constants", "constants")
	compileOnly.TypeTable = types.CloneTable(artifact.TypeTable)
	emitter := newPackageEmitter(&compileOnly, nil)
	lowerer := &slotLowerer{packageEmitter: emitter}
	typedOperand, err := lowerer.expression(literal(float64Type, numeric("1/2", "Float64", false), false))
	if err != nil {
		t.Fatal(err)
	}
	untypedOperand, err := lowerer.expression(literal(float64Type, numeric("1/2", "Float64", true), true))
	if err != nil {
		t.Fatal(err)
	}
	if typedOperand[0].Index == untypedOperand[0].Index {
		t.Fatal("typed and untyped float literals share compile-time constant identity")
	}
	if !compileOnly.Constants[untypedOperand[0].Index].Untyped {
		t.Fatal("untyped literal lost its compile-time marker")
	}
}

func TestAnonymousLiteralPoolDeduplicatesOnlyUnnamedLiterals(t *testing.T) {
	integer := types.Builtin(types.PrimitiveInt)
	artifact := ir.NewArtifact("example/main", "main")
	emitter := newPackageEmitter(&artifact, nil)
	lowerer := &slotLowerer{packageEmitter: emitter}
	literal := hir.Expression{Kind: hir.ExprLiteral, Type: integer, Value: constant.Scalar("7")}
	if _, err := lowerer.expression(literal); err != nil {
		t.Fatal(err)
	}
	if _, err := lowerer.expression(literal); err != nil {
		t.Fatal(err)
	}
	if len(artifact.Constants) != 1 {
		t.Fatalf("anonymous literals were not deduplicated: %d constants", len(artifact.Constants))
	}
	named := literal
	named.ConstantID = "const.seven"
	if _, err := lowerer.expression(named); err != nil {
		t.Fatal(err)
	}
	if len(artifact.Constants) != 2 {
		t.Fatalf("named literal was incorrectly merged into anonymous pool: %d constants", len(artifact.Constants))
	}
}

func TestLowerKeepsMutableByteSliceLiteralsIndependent(t *testing.T) {
	byteSlice := testHIRType("Slice<Uint8>")
	literal := func() hir.Expression {
		return hir.Expression{Kind: hir.ExprLiteral, Type: byteSlice, Value: constant.String("x", "Slice<Uint8>", false).Ref()}
	}
	program := hir.Program{
		ModulePath: "example/bytes",
		Package:    "bytes",
		Functions: []hir.Function{{
			ID:        "fn.main",
			Name:      "main",
			Signature: testHIRSignature("function() Slice<Uint8>"),
			Locals:    []hir.Local{{ID: "left", Type: byteSlice}, {ID: "right", Type: byteSlice}},
			Body: []hir.Statement{
				{Kind: hir.StmtStoreLocal, Local: "left", Expr: literal()},
				{Kind: hir.StmtStoreLocal, Local: "right", Expr: literal()},
				{Kind: hir.StmtReturn, Results: []hir.Expression{{Kind: hir.ExprLocal, Local: "right", Type: byteSlice}}},
			},
		}},
	}
	artifact, err := lowerTestProgram(t, program)
	if err != nil {
		t.Fatal(err)
	}

	byteSliceConstants := 0
	for _, constant := range artifact.Constants {
		if types.View(&artifact.TypeTable, constant.Type).Underlying().Kind == types.Slice {
			byteSliceConstants++
		}
	}
	if byteSliceConstants != 2 {
		t.Fatalf("independent byte literals were pooled: %d constants (%#v)", byteSliceConstants, artifact.Constants)
	}
	operandsByLocal := make(map[string]ir.Operand, 2)
	code := artifact.Functions[0].Code
	for _, instruction := range code.Instructions {
		if instruction.Op != ir.OpStoreLocal {
			continue
		}
		payload, err := code.Descriptors.Payload(instruction.Op, instruction.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		local := payload.(ir.LocalPayload).Local
		inputs := code.Operands[instruction.Operands].Inputs
		if len(inputs) != 1 || inputs[0].Kind != ir.OperandConstant {
			t.Fatalf("literal store %s lost its constant operand: %#v", local, inputs)
		}
		operandsByLocal[local] = inputs[0]
	}
	if operandsByLocal["left"].Index == operandsByLocal["right"].Index {
		t.Fatalf("mutable literals share constant operand: %#v", operandsByLocal)
	}
}
