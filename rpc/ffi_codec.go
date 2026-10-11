package rpc

import (
	"errors"
)

// FFIProtocol identifies the binary MRPC envelope carried by FFIRoute.
const FFIProtocol = "minigo.mrpc.ffi/v3"

type ffiRequest struct {
	Version      string
	Operation    string
	Target       string
	Lease        uint64
	Contract     Contract
	Options      BindOptions
	Method       Method
	Receiver     *ResourceRef
	Payload      []byte
	Resource     *ResourceRef
	RequestID    uint64
	Code         Code
	Message      string
	TimeoutNanos int64
}

type ffiResponse struct {
	Version   string
	Operation string
	Lease     uint64
	RequestID uint64
	Method    Method
	Receiver  *ResourceRef
	Payload   []byte
	Code      Code
	Message   string
}

func decodeFFIRequest(data []byte, limits Limits) (ffiRequest, error) {
	limits = normalizeLimits(limits)
	if len(data) > limits.MaxMessageBytes {
		return ffiRequest{}, errors.New("MRPC FFI request exceeds message limit")
	}
	decoder := newWireDecoder(data, limits.MaxMessageBytes)
	request := ffiRequest{}
	var err error
	request.Version, err = decoder.String()
	if err == nil {
		request.Operation, err = decoder.String()
	}
	if err == nil {
		request.Target, err = decoder.String()
	}
	if err == nil {
		request.Lease, err = decoder.Uint()
	}
	if err == nil {
		request.Contract.Protocol, err = decoder.String()
	}
	var methodCount uint64
	if err == nil {
		methodCount, err = decoder.Uint()
	}
	if err == nil && methodCount > uint64(limits.MaxMethods) {
		err = errors.New("MRPC FFI contract exceeds method limit")
	}
	if err == nil {
		request.Contract.Methods = make([]Method, int(methodCount))
		for index := range request.Contract.Methods {
			request.Contract.Methods[index], err = decodeWireMethod(decoder)
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		request.Options.AffinityKey, err = decoder.String()
	}
	if err == nil {
		request.Options.Labels, err = decodeWireLabels(decoder, limits.MaxMethods)
	}
	if err == nil {
		request.Method, err = decodeWireMethod(decoder)
	}
	if err == nil {
		request.Receiver, err = decodeWireResource(decoder)
	}
	if err == nil {
		request.Payload, err = decoder.rawView()
	}
	if err == nil {
		request.Resource, err = decodeWireResource(decoder)
	}
	if err == nil {
		request.RequestID, err = decoder.Uint()
	}
	if err == nil {
		var code string
		code, err = decoder.String()
		request.Code = Code(code)
	}
	if err == nil {
		request.Message, err = decoder.String()
	}
	if err == nil {
		request.TimeoutNanos, err = decoder.Int()
	}
	if err == nil {
		err = decoder.Done()
	}
	return request, err
}

func encodeFFIResponse(response ffiResponse) []byte {
	var encoder wireEncoder
	encoder.String(response.Version)
	encoder.String(response.Operation)
	encoder.Uint(response.Lease)
	encoder.Uint(response.RequestID)
	encodeWireMethod(&encoder, response.Method)
	encodeWireResource(&encoder, response.Receiver)
	encoder.Raw(response.Payload)
	encoder.String(string(response.Code))
	encoder.String(response.Message)
	return encoder.data
}
