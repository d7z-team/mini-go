package bytecode

import (
	"encoding/json"
	"testing"
)

func FuzzCallIntrinsicPayload(f *testing.F) {
	f.Add("reflect.type_of", int8(1), int8(1), false)
	f.Add("reflect.value_call", int8(3), int8(3), false)
	f.Add("reflect.value_set", int8(2), int8(2), false)
	f.Add("reflect.unknown", int8(0), int8(0), false)
	f.Add("reflect.type_of", int8(1), int8(1), true)

	f.Fuzz(func(t *testing.T, id string, argInput, resultInput int8, extra bool) {
		if len(id) > 256 {
			t.Skip()
		}
		argCount, resultCount := int(argInput), int(resultInput)
		payload := map[string]any{
			"id": id, "arg_count": argCount, "result_count": resultCount,
		}
		if extra {
			payload["unknown"] = true
		}
		raw, err := json.Marshal(struct {
			Op      string         `json:"op"`
			Payload map[string]any `json:"payload"`
		}{OpCallIntrinsic.String(), payload})
		if err != nil {
			t.Fatal(err)
		}
		var instruction Instruction
		err = json.Unmarshal(raw, &instruction)
		if err == nil {
			err = validateInstruction("functions[0].instructions[0]", &instruction)
		}
		descriptor, known := Intrinsic(IntrinsicID(id))
		valid := known && !extra && argCount == descriptor.ArgCount && resultCount == descriptor.ResultCount
		if !valid {
			if err == nil {
				t.Fatalf("invalid intrinsic payload was accepted: %#v", instruction.Payload)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid intrinsic payload was rejected: %v", err)
		}
		inputs, outputs, err := instructionArity(&instruction)
		if err != nil || inputs != descriptor.ArgCount || outputs != descriptor.ResultCount {
			t.Fatalf("intrinsic arity = inputs %d outputs %d err %v", inputs, outputs, err)
		}
	})
}
