package bytecode

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestInstructionDescriptorsRoundTripAndCloneWithoutSharingTables(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{Code: testSlotCode(nil, []Instruction{
		{Op: OpAddressOf, Payload: AddressPayload{Kind: "local", Local: "x", Path: []AddressPathSegment{{Kind: "field", Field: "Value"}}}},
		{Op: OpMakeClosure, Payload: ClosurePayload{Function: "f", Captures: []AddressPayload{{Kind: "local", Local: "x", Path: []AddressPathSegment{{Kind: "field", Field: "Value"}}}}}},
		{Op: OpMakeStruct, Payload: MakeStructPayload{Fields: []string{"Value"}}},
		{Op: OpSelect, Payload: SelectPayload{Index: "i", Cases: []SelectCase{{Channel: "ch", Send: "x"}}}},
		{Op: OpTypeDispatch, Payload: TypeDispatchPayload{Subject: "x", Default: "done", Cases: []TypeCase{{Label: "nil", Original: true}}}},
		{Op: OpTypeDispatch, Payload: TypeDispatchPayload{Subject: "x", Default: "done", Cases: []TypeCase{}}},
		{Op: OpDeferPush, Payload: DeferPayload{}},
	}, make([][2][]uint32, 7))}}
	before, err := CanonicalJSON(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Artifact
	if err := json.Unmarshal(before, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(artifact, decoded) {
		t.Fatalf("descriptor round trip differs: %#v", decoded.Functions)
	}
	cloned := CloneArtifact(artifact)
	clonedBytes, err := CanonicalJSON(&cloned)
	if err != nil || !bytes.Equal(before, clonedBytes) {
		t.Fatalf("clone changed descriptor shape: %v", err)
	}
	ins, err := cloned.Functions[0].Operations()
	if err != nil {
		t.Fatal(err)
	}
	ins[0].Payload.(AddressPayload).Path[0].Field = "Changed"
	ins[1].Payload.(ClosurePayload).Captures[0].Path[0].Field = "Changed"
	ins[2].Payload.(MakeStructPayload).Fields[0] = "Changed"
	ins[3].Payload.(SelectPayload).Cases[0].Send = "Changed"
	ins[4].Payload.(TypeDispatchPayload).Cases[0].Label = "Changed"
	after, err := CanonicalJSON(&artifact)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("clone shared a descriptor table: %v", err)
	}
}

func TestInstructionDecodeRejectsMalformedPayloadWithoutReplacingDestination(t *testing.T) {
	for _, raw := range []string{
		`{"op":"return","payload":{"result_count":0,"unknown":1}}`,
		`{"op":"return","payload":{"result_count":0} {}}`,
		`{"op":"return","payload":{"result_count":0,}}`,
		`{"op":"return","payload":{"result_count":0}`,
		`{"op":"pop","payload":{"unexpected":true}}`,
	} {
		before := Instruction{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}}
		instruction := before
		if err := json.Unmarshal([]byte(raw), &instruction); err == nil {
			t.Fatalf("accepted malformed wire instruction %s", raw)
		}
		if !reflect.DeepEqual(before, instruction) {
			t.Fatal("failed decode replaced its destination")
		}
	}
}

func TestDescriptorAppendRejectsPointerPayloadWithoutChangingTables(t *testing.T) {
	var missing *ReturnPayload
	for _, payload := range []Payload{missing, &ReturnPayload{}} {
		tables := DescriptorTables{Return: []ReturnPayload{{ResultCount: 1}}}
		if _, err := tables.Append(payload); err == nil {
			t.Fatal("accepted descriptor with unsupported pointer storage")
		}
		if !reflect.DeepEqual(tables, DescriptorTables{Return: []ReturnPayload{{ResultCount: 1}}}) {
			t.Fatal("failed append changed descriptor tables")
		}
	}
}
