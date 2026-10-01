package constant

import (
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func TestDecimalDivisionMatchesRandomLargeIntegers(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for range 2000 {
		left := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1), uint(1+random.Intn(4096))))
		right := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1), uint(1+random.Intn(2048))))
		right.Add(right, big.NewInt(1))
		quotient, remainder, ok := DivideUnsignedDecimal(left.String(), right.String())
		wantQuotient, wantRemainder := new(big.Int), new(big.Int)
		wantQuotient.QuoRem(left, right, wantRemainder)
		if !ok || quotient != wantQuotient.String() || remainder != wantRemainder.String() {
			t.Fatalf("%s / %s: got %s remainder %s (%v), want %s remainder %s", left, right, quotient, remainder, ok, wantQuotient, wantRemainder)
		}
	}
}

func BenchmarkDivideLargeDecimals(b *testing.B) {
	left := Pow2UnsignedDecimal(1074)
	right := "9007199254740989"
	for b.Loop() {
		_, _, _ = DivideUnsignedDecimal(left, right)
	}
}

func TestDecimalGCDMatchesBigIntAcrossMachineIntegerBoundary(t *testing.T) {
	values := []string{
		"0", "1", "7540113804746346429", "12200160415121876738",
		"18446744073709551615", "18446744073709551616", "18446744073709551617",
		"340282366920938463463374607431768211455",
	}
	for _, left := range values {
		for _, right := range values {
			a, _ := new(big.Int).SetString(left, 10)
			c, _ := new(big.Int).SetString(right, 10)
			want := new(big.Int).GCD(nil, nil, a, c).String()
			if got := GCDUnsignedDecimal(left, right); got != want {
				t.Fatalf("gcd(%s, %s) = %s, want %s", left, right, got, want)
			}
		}
	}
}

func BenchmarkGCDUnsignedDecimal(b *testing.B) {
	for _, operands := range []struct{ name, left, right string }{
		{"integer", "42", "1"},
		{"wide_integer", "18446744073709551615", "1"},
		{"arbitrary_integer", "340282366920938463463374607431768211455", "1"},
		{"fibonacci", "12200160415121876738", "7540113804746346429"},
		{"large", "340282366920938463463374607431768211455", "18446744073709551615"},
		{"wide_fibonacci", "453973694165307953197296969697410619233826", "280571172992510140037611932413038677189525"},
	} {
		b.Run(operands.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = GCDUnsignedDecimal(operands.left, operands.right)
			}
		})
	}
}

func TestGCDMatchesLargeEuclideanChains(t *testing.T) {
	random := rand.New(rand.NewSource(9))
	for range 256 {
		left := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1), uint(1+random.Intn(2048))))
		right := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1), uint(1+random.Intn(2048))))
		factor := new(big.Int).Rand(random, new(big.Int).Lsh(big.NewInt(1), 128))
		left.Mul(left, factor)
		right.Mul(right, factor)
		want := new(big.Int).GCD(nil, nil, left, right).String()
		if got := GCDUnsignedDecimal(left.String(), right.String()); got != want {
			t.Fatalf("gcd(%s, %s) = %s, want %s", left, right, got, want)
		}
	}
	left, right := big.NewInt(0), big.NewInt(1)
	for index := 0; index < 600; index++ {
		left, right = right, new(big.Int).Add(left, right)
		if index%37 == 0 {
			if got := GCDUnsignedDecimal(left.String(), right.String()); got != "1" {
				t.Fatalf("consecutive Fibonacci gcd at %d = %s", index, got)
			}
		}
	}
}

func TestPow2UnsignedDecimalMatchesBigInteger(t *testing.T) {
	for bits := -1; bits <= 1100; bits++ {
		exponent := bits
		if exponent < 0 {
			exponent = 0
		}
		want := new(big.Int).Lsh(big.NewInt(1), uint(exponent)).String()
		if got := Pow2UnsignedDecimal(bits); got != want {
			t.Fatalf("2^%d = %s, want %s", bits, got, want)
		}
	}
	for _, bits := range []int{4096, 16384} {
		want := new(big.Int).Lsh(big.NewInt(1), uint(bits)).String()
		if got := Pow2UnsignedDecimal(bits); got != want {
			t.Fatalf("2^%d differs from arbitrary precision integer", bits)
		}
	}
}

func BenchmarkPow2UnsignedDecimal(b *testing.B) {
	for _, bits := range []int{53, 1024, 4096} {
		b.Run(strconv.Itoa(bits), func(b *testing.B) {
			for b.Loop() {
				_ = Pow2UnsignedDecimal(bits)
			}
		})
	}
}

func TestLongDecimalDivisionAndGCDMatchBigInt(t *testing.T) {
	values := []string{"1" + strings.Repeat("0", 600), strings.Repeat("9", 309), strings.Repeat("1234567890", 60)}
	divisors := []string{"100000000000000000001", strings.Repeat("7", 153), "1" + strings.Repeat("0", 308)}
	for _, left := range values {
		for _, right := range divisors {
			a, _ := new(big.Int).SetString(left, 10)
			b, _ := new(big.Int).SetString(right, 10)
			wantQuotient, wantRemainder := new(big.Int), new(big.Int)
			wantQuotient.QuoRem(a, b, wantRemainder)
			quotient, remainder, ok := DivideUnsignedDecimal(left, right)
			if !ok || quotient != wantQuotient.String() || remainder != wantRemainder.String() {
				t.Fatalf("division of %d digits by %d digits = %s remainder %s, %v", len(left), len(right), quotient, remainder, ok)
			}
			wantGCD := new(big.Int).GCD(nil, nil, a, b)
			if got := GCDUnsignedDecimal(left, right); got != wantGCD.String() {
				t.Fatalf("gcd = %s, want %s", got, wantGCD)
			}
		}
	}
}

func TestDecimalNormalizationPreservesWhitespaceSignsAndRejectsNonDigits(t *testing.T) {
	for _, test := range []struct {
		input, signed, unsigned string
	}{
		{"0", "0", "0"},
		{"00042", "42", "42"},
		{" 000 ", "0", "0"},
		{"\u200342\u2003", "42", "42"},
		{"+ 0042", "42", ""},
		{"-\u20030042", "-42", ""},
		{"- 000", "0", ""},
		{"", "", ""},
		{"+", "", ""},
		{"--1", "", ""},
		{"+-1", "", ""},
		{"1e0", "", ""},
		{"1.0", "", ""},
		{"1 2", "", ""},
		{"\x0042", "", ""},
		{"\xff42", "", ""},
		{"４２", "", ""},
	} {
		for name, normalize := range map[string]func(string) (string, bool){"signed": NormalizeSignedDecimal, "unsigned": normalizeUnsignedDecimal} {
			want := test.signed
			if name == "unsigned" {
				want = test.unsigned
			}
			got, ok := normalize(test.input)
			if got != want || ok != (want != "") {
				t.Fatalf("%s(%q) = %q, %v; want %q", name, test.input, got, ok, want)
			}
		}
	}
}

func TestDecimalDivisionPreservesArbitraryPrecision(t *testing.T) {
	value := "123456789012345678901234567890"
	quotient, remainder, ok := DivideSignedDecimal(value, "97")
	if !ok {
		t.Fatal("DivideSignedDecimal failed")
	}
	rebuilt, _ := MultiplySignedDecimal(quotient, "97")
	rebuilt, _ = AddSignedDecimal(rebuilt, remainder)
	if rebuilt != value {
		t.Fatalf("division identity = %s, want %s", rebuilt, value)
	}
	quotient, remainder, ok = DivideSignedDecimal("-100", "7")
	if !ok || quotient != "-14" || remainder != "-2" {
		t.Fatalf("negative division = %s remainder %s", quotient, remainder)
	}
}

func TestDecimalArithmeticMatchesBigInt(t *testing.T) {
	values := []string{
		"0", "1", "9", "10", "97", "18446744073709551615",
		"18446744073709551616", "1234567890123456789012345678901234567890",
		strings.Repeat("9", 1000), "1" + strings.Repeat("0", 1000),
	}
	for _, left := range values {
		for _, right := range values {
			leftBig, _ := new(big.Int).SetString(left, 10)
			rightBig, _ := new(big.Int).SetString(right, 10)
			product, ok := MultiplySignedDecimal(left, right)
			wantProduct := new(big.Int).Mul(leftBig, rightBig).String()
			if !ok || product != wantProduct {
				t.Fatalf("%s * %s = %q, %v; want %q", left, right, product, ok, wantProduct)
			}
			if right == "0" {
				continue
			}
			quotient, remainder, ok := DivideUnsignedDecimal(left, right)
			wantQuotient, wantRemainder := new(big.Int), new(big.Int)
			wantQuotient.QuoRem(leftBig, rightBig, wantRemainder)
			if !ok || quotient != wantQuotient.String() || remainder != wantRemainder.String() {
				t.Fatalf("%s / %s = %q remainder %q, %v; want %q remainder %q", left, right, quotient, remainder, ok, wantQuotient, wantRemainder)
			}
		}
	}
}

func BenchmarkMultiplyLargeDecimals(b *testing.B) {
	left, right := strings.Repeat("9", 324), strings.Repeat("123456789", 36)
	for b.Loop() {
		_, _ = MultiplySignedDecimal(left, right)
	}
}

func FuzzDecimalArithmetic(f *testing.F) {
	for _, seed := range [][2]string{{"1", "1"}, {"18446744073709551616", "97"}, {"123456789012345678901234567890", "9876543210987654321"}, {"1000000000000000000000000001", "500000000000000000000000001"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, left, right string) {
		left, leftOK := normalizeUnsignedDecimal(left)
		right, rightOK := normalizeUnsignedDecimal(right)
		if !leftOK || !rightOK || len(left) > 256 || len(right) > 256 {
			return
		}
		leftBig, _ := new(big.Int).SetString(left, 10)
		rightBig, _ := new(big.Int).SetString(right, 10)
		product, ok := MultiplySignedDecimal(left, right)
		if !ok || product != new(big.Int).Mul(leftBig, rightBig).String() {
			t.Fatalf("product mismatch for %q and %q", left, right)
		}
		if right == "0" {
			return
		}
		quotient, remainder, ok := DivideUnsignedDecimal(left, right)
		wantQuotient, wantRemainder := new(big.Int), new(big.Int)
		wantQuotient.QuoRem(leftBig, rightBig, wantRemainder)
		if !ok || quotient != wantQuotient.String() || remainder != wantRemainder.String() {
			t.Fatalf("division mismatch for %q and %q", left, right)
		}
	})
}

func TestDecimalBitwiseUsesSignedArbitraryPrecision(t *testing.T) {
	huge := "1267650600228229401496703205376"
	if got, ok := BitwiseSignedDecimal("&", "-1", huge); !ok || got != huge {
		t.Fatalf("-1 & huge = %s", got)
	}
	if got, ok := BitwiseSignedDecimal("^", "-1", huge); !ok || got != "-1267650600228229401496703205377" {
		t.Fatalf("-1 ^ huge = %s", got)
	}
}
