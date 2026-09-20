package runtime

import (
	"errors"
	"math/bits"
)

var sha256Round = [64]uint32{
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

func sha256Block(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	array, ok := args[0].Data.(*vmArray)
	if !ok || array.Len != 8 {
		return nil, errors.New("sha256 block state must contain 8 uint32 values")
	}
	state := array.values()
	input, ok := args[1].Data.(*vmSlice)
	if !ok || input == nil || input.Len%64 != 0 {
		return nil, errors.New("sha256 block input length must be a multiple of 64")
	}
	var data []byte
	if input.ByteBacked {
		data = input.bytes()
	} else {
		data = make([]byte, input.Len)
		for i := range data {
			if i&255 == 0 {
				if err := ctx.cancellationError(); err != nil {
					return nil, err
				}
			}
			value, err := numericAsUint64(input.valueAt(i))
			if err != nil || value > 255 {
				return nil, errors.New("sha256 block input contains a non-byte value")
			}
			data[i] = byte(value)
		}
	}
	var digest [8]uint32
	for i, value := range state {
		n, err := numericAsUint64(value)
		if err != nil {
			return nil, errors.New("sha256 block state contains a non-uint32 value")
		}
		digest[i] = uint32(n)
	}
	var words [64]uint32
	for len(data) >= 64 {
		if err := ctx.cancellationError(); err != nil {
			return nil, err
		}
		for i := 0; i < 16; i++ {
			j := i * 4
			words[i] = uint32(data[j])<<24 | uint32(data[j+1])<<16 | uint32(data[j+2])<<8 | uint32(data[j+3])
		}
		for i := 16; i < 64; i++ {
			v1, v2 := words[i-2], words[i-15]
			s0 := bits.RotateLeft32(v1, -17) ^ bits.RotateLeft32(v1, -19) ^ v1>>10
			s1 := bits.RotateLeft32(v2, -7) ^ bits.RotateLeft32(v2, -18) ^ v2>>3
			words[i] = s0 + words[i-7] + s1 + words[i-16]
		}
		a, b, c, d := digest[0], digest[1], digest[2], digest[3]
		e, f, g, h := digest[4], digest[5], digest[6], digest[7]
		for i := 0; i < 64; i++ {
			t1 := h + (bits.RotateLeft32(e, -6) ^ bits.RotateLeft32(e, -11) ^ bits.RotateLeft32(e, -25)) + ((e & f) ^ (^e & g)) + sha256Round[i] + words[i]
			t2 := (bits.RotateLeft32(a, -2) ^ bits.RotateLeft32(a, -13) ^ bits.RotateLeft32(a, -22)) + ((a & b) ^ (a & c) ^ (b & c))
			h, g, f, e, d, c, b, a = g, f, e, d+t1, c, b, a, t1+t2
		}
		digest[0] += a
		digest[1] += b
		digest[2] += c
		digest[3] += d
		digest[4] += e
		digest[5] += f
		digest[6] += g
		digest[7] += h
		data = data[64:]
	}
	if err := ctx.vm.chargeRuntimeObject(len(state), 0); err != nil {
		return nil, err
	}
	out := make([]vmValue, len(state))
	for i := range out {
		out[i] = newUnsignedVMValue(state[i].Type, uint64(digest[i]))
	}
	return []vmValue{newVMValue(args[0].Type, out)}, nil
}
