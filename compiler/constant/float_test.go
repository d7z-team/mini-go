package constant

import (
	"math"
	"math/big"
	"strings"
	"testing"
)

func TestFloatRepresentabilityAtExactOverflowMidpoints(t *testing.T) {
	for _, bits := range []int{32, 64} {
		largest, halfULP := math.MaxFloat64, math.Ldexp(1, 970)
		if bits == 32 {
			largest, halfULP = math.MaxFloat32, math.Ldexp(1, 103)
		}
		midpoint := new(big.Rat).Add(new(big.Rat).SetFloat64(largest), new(big.Rat).SetFloat64(halfULP))
		for _, delta := range []int64{-1, 0, 1} {
			for _, negative := range []bool{false, true} {
				exact := new(big.Rat).Add(midpoint, big.NewRat(delta, 1))
				if negative {
					exact.Neg(exact)
				}
				value := Rational{Numerator: exact.Num().String(), Denominator: exact.Denom().String()}
				if got := FloatRepresentable(value, bits); got != (delta < 0) {
					t.Fatalf("bits=%d delta=%d negative=%v: representable=%v", bits, delta, negative, got)
				}
			}
		}
		for _, exact := range []*big.Rat{big.NewRat(0, 1), big.NewRat(-42, 7), new(big.Rat).Quo(midpoint, big.NewRat(2, 1))} {
			value := Rational{Numerator: exact.Num().String(), Denominator: exact.Denom().String()}
			if !FloatRepresentable(value, bits) {
				t.Fatalf("finite rational rejected: %v (%d bits)", exact, bits)
			}
		}
	}
}

func TestRoundRationalFloatPreservesCanonicalBinaryValue(t *testing.T) {
	patterns := []uint64{0, 1, 2, 0xfffffffffffff, 0x10000000000000, 0x3ff0000000000000, 0x7fefffffffffffff}
	state := uint64(1)
	for range 128 {
		state = state*6364136223846793005 + 1442695040888963407
		patterns = append(patterns, state&0x7fefffffffffffff)
	}
	for _, pattern := range patterns {
		for _, negative := range []bool{false, true} {
			value := math.Float64frombits(pattern)
			if negative {
				value = -value
			}
			want := new(big.Rat).SetFloat64(value)
			input := Rational{Numerator: want.Num().String(), Denominator: want.Denom().String()}
			got, ok := RoundRationalFloat(input, 64)
			if !ok || got != input {
				t.Fatalf("bits %x negative %v: got %+v (%v), want %+v", pattern, negative, got, ok, input)
			}
		}
	}
}

func BenchmarkRoundRationalFloatSubnormal(b *testing.B) {
	value, ok := ParseRationalLiteral("1e-308")
	if !ok {
		b.Fatal("invalid benchmark constant")
	}
	for b.Loop() {
		_, _ = RoundRationalFloat(value, 64)
	}
}

func TestBinaryExponentMatchesExactRationalMagnitude(t *testing.T) {
	for power := 0; power <= 1200; power += 37 {
		large := "7" + strings.Repeat("0", power)
		for _, pair := range [][2]string{{large, "3"}, {"3", large}, {large, large}} {
			rat, ok := new(big.Rat).SetString(pair[0] + "/" + pair[1])
			if !ok {
				t.Fatal("invalid test rational")
			}
			value := new(big.Float).SetPrec(uint((power+1)*4 + 64)).SetRat(rat)
			want := value.MantExp(nil) - 1
			if got := estimateBinaryExponent(pair[0], pair[1]); got != want {
				t.Fatalf("power %d: exponent %d, want %d", power, got, want)
			}
		}
	}
}

func BenchmarkRationalFloatLargeExponent(b *testing.B) {
	value, ok := ParseRationalLiteral("1e308")
	if !ok {
		b.Fatal("invalid benchmark constant")
	}
	for b.Loop() {
		_, _ = RationalFloat(value, 64)
	}
}
