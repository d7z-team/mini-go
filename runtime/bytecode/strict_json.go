package bytecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

func strictUnmarshal(raw json.RawMessage, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// DecodeInstructionPayload decodes one instruction payload and rejects
// unknown fields and trailing JSON values.
func DecodeInstructionPayload(raw json.RawMessage, out any) error {
	return decodeInstructionPayload(raw, out, false)
}

func decodeInstructionPayload(raw json.RawMessage, out any, validJSON bool) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
		validJSON = true
	}
	if validJSON || json.Valid(raw) {
		if handled, err := decodeFixedPayload(raw, out); handled && err == nil {
			return nil
		}
	}
	return strictUnmarshal(raw, out)
}
