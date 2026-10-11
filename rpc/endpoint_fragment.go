package rpc

import (
	"encoding/binary"
	"errors"
)

const (
	endpointFragmentMagic   = "MGRF"
	endpointFragmentVersion = byte(1)
	maxFragmentHeaderBytes  = len(endpointFragmentMagic) + 1 + 3*binary.MaxVarintLen64
)

type endpointFragment struct {
	messageID uint64
	total     uint64
	offset    uint64
	data      []byte
}

type endpointAssembly struct {
	messageID uint64
	data      []byte
	received  int
}

func encodeEndpointFragmentTo(dst []byte, messageID uint64, total, offset int, data []byte, maxFrameBytes int) ([]byte, error) {
	if total <= 0 || offset < 0 || len(data) == 0 || offset > total || len(data) > total-offset || messageID == 0 && (offset != 0 || len(data) != total) {
		return nil, errors.New("invalid rpc fragment")
	}
	var encoder wireEncoder
	if cap(dst) < maxFragmentHeaderBytes+len(data) {
		dst = make([]byte, 0, maxFragmentHeaderBytes+len(data))
	} else {
		dst = dst[:0]
	}
	encoder.data = dst
	encoder.data = append(encoder.data, endpointFragmentMagic...)
	encoder.data = append(encoder.data, endpointFragmentVersion)
	encoder.Uint(messageID)
	encoder.Uint(uint64(total))
	encoder.Uint(uint64(offset))
	encoder.data = append(encoder.data, data...)
	if len(encoder.data) > maxFrameBytes {
		return nil, errors.New("rpc fragment exceeds frame limit")
	}
	return encoder.data, nil
}

func decodeEndpointFragment(payload []byte, limits Limits) (endpointFragment, error) {
	limits = normalizeLimits(limits)
	if len(payload) == 0 || len(payload) > limits.MaxFrameBytes || len(payload) <= len(endpointFragmentMagic)+1 {
		return endpointFragment{}, errors.New("invalid rpc fragment size")
	}
	decoder := newWireDecoder(payload, limits.MaxFrameBytes)
	magic, err := decoder.fixed(len(endpointFragmentMagic))
	if err != nil || string(magic) != endpointFragmentMagic {
		return endpointFragment{}, errors.New("invalid rpc fragment header")
	}
	version, err := decoder.byte()
	if err != nil || version != endpointFragmentVersion {
		return endpointFragment{}, errors.New("invalid rpc fragment header")
	}
	messageID, err := decoder.Uint()
	if err != nil {
		return endpointFragment{}, errors.New("invalid rpc fragment message ID")
	}
	total, err := decoder.Uint()
	if err != nil {
		return endpointFragment{}, errors.New("invalid rpc fragment total")
	}
	fragmentOffset, err := decoder.Uint()
	if err != nil {
		return endpointFragment{}, errors.New("invalid rpc fragment offset")
	}
	chunk, err := decoder.fixed(decoder.Remaining())
	if err != nil {
		return endpointFragment{}, errors.New("invalid rpc fragment data")
	}
	if total == 0 || len(chunk) == 0 || total > uint64(limits.MaxMessageBytes) ||
		total > uint64(limits.MaxInFlightBytes) || fragmentOffset > total || uint64(len(chunk)) > total-fragmentOffset {
		return endpointFragment{}, errors.New("invalid rpc fragment bounds")
	}
	if messageID == 0 && (fragmentOffset != 0 || uint64(len(chunk)) != total) {
		return endpointFragment{}, errors.New("rpc control fragment must be complete")
	}
	return endpointFragment{messageID: messageID, total: total, offset: fragmentOffset, data: chunk}, nil
}

func (assembly *endpointAssembly) append(fragment endpointFragment, previousID *uint64) ([]byte, error) {
	if fragment.messageID == 0 {
		if fragment.offset != 0 || uint64(len(fragment.data)) != fragment.total {
			return nil, errors.New("rpc control fragment must be complete")
		}
		return fragment.data, nil
	}
	if assembly.data == nil {
		if fragment.offset != 0 || fragment.messageID != *previousID+1 {
			return nil, errors.New("rpc fragment sequence is not strictly increasing")
		}
		assembly.messageID = fragment.messageID
		assembly.data = make([]byte, int(fragment.total))
		assembly.received = 0
	} else if fragment.messageID != assembly.messageID || fragment.total != uint64(len(assembly.data)) {
		return nil, errors.New("rpc fragment changed an incomplete message")
	}
	if fragment.offset != uint64(assembly.received) {
		return nil, errors.New("rpc fragment offset is not contiguous")
	}
	copy(assembly.data[assembly.received:], fragment.data)
	assembly.received += len(fragment.data)
	if assembly.received != len(assembly.data) {
		return nil, nil
	}
	message := assembly.data
	*previousID = assembly.messageID
	*assembly = endpointAssembly{}
	return message, nil
}
