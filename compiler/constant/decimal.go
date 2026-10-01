package constant

import (
	"strconv"
	"strings"
)

func normalizeUnsignedDecimal(text string) (string, bool) {
	if len(text) != 0 && (text[0] <= ' ' || text[len(text)-1] <= ' ' || text[0] >= 0x80 || text[len(text)-1] >= 0x80) {
		text = strings.TrimSpace(text)
	}
	if text == "" {
		return "", false
	}
	first := 0
	for first < len(text) && text[first] == '0' {
		first++
	}
	if first == len(text) {
		return "0", true
	}
	for i := first; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return "", false
		}
	}
	return text[first:], true
}

func NormalizeSignedDecimal(text string) (string, bool) {
	if len(text) != 0 && (text[0] <= ' ' || text[len(text)-1] <= ' ' || text[0] >= 0x80 || text[len(text)-1] >= 0x80) {
		text = strings.TrimSpace(text)
	}
	if text == "" {
		return "", false
	}
	sign := ""
	if text[0] == '+' || text[0] == '-' {
		if text[0] == '-' {
			sign = "-"
		}
		text = text[1:]
	}
	unsigned, ok := normalizeUnsignedDecimal(text)
	if !ok {
		return "", false
	}
	if unsigned == "0" {
		return "0", true
	}
	return sign + unsigned, true
}

func SignedDecimalInt64(text string) (int64, bool) {
	text, ok := NormalizeSignedDecimal(text)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil
}

func CompareSignedDecimal(left, right string) int {
	left, leftOK := NormalizeSignedDecimal(left)
	right, rightOK := NormalizeSignedDecimal(right)
	if !leftOK || !rightOK {
		return strings.Compare(left, right)
	}
	leftNegative := strings.HasPrefix(left, "-")
	rightNegative := strings.HasPrefix(right, "-")
	if leftNegative && !rightNegative {
		return -1
	}
	if !leftNegative && rightNegative {
		return 1
	}
	comparison := compareCanonicalDecimal(strings.TrimPrefix(left, "-"), strings.TrimPrefix(right, "-"))
	if leftNegative {
		return -comparison
	}
	return comparison
}

func NegateSignedDecimal(text string) (string, bool) {
	text, ok := NormalizeSignedDecimal(text)
	if !ok {
		return "", false
	}
	if text == "0" {
		return "0", true
	}
	if strings.HasPrefix(text, "-") {
		return strings.TrimPrefix(text, "-"), true
	}
	return "-" + text, true
}

func AddSignedDecimal(left, right string) (string, bool) {
	left, leftOK := NormalizeSignedDecimal(left)
	right, rightOK := NormalizeSignedDecimal(right)
	if !leftOK || !rightOK {
		return "", false
	}
	leftNegative := strings.HasPrefix(left, "-")
	rightNegative := strings.HasPrefix(right, "-")
	leftMagnitude := strings.TrimPrefix(left, "-")
	rightMagnitude := strings.TrimPrefix(right, "-")
	if leftNegative == rightNegative {
		sum := addCanonicalDecimal(leftMagnitude, rightMagnitude)
		if leftNegative && sum != "0" {
			return "-" + sum, true
		}
		return sum, true
	}
	comparison := compareCanonicalDecimal(leftMagnitude, rightMagnitude)
	if comparison == 0 {
		return "0", true
	}
	if comparison > 0 {
		difference := subtractCanonicalDecimal(leftMagnitude, rightMagnitude)
		if leftNegative && difference != "0" {
			return "-" + difference, true
		}
		return difference, true
	}
	difference := subtractCanonicalDecimal(rightMagnitude, leftMagnitude)
	if rightNegative && difference != "0" {
		return "-" + difference, true
	}
	return difference, true
}

func SubtractSignedDecimal(left, right string) (string, bool) {
	negated, ok := NegateSignedDecimal(right)
	if !ok {
		return "", false
	}
	return AddSignedDecimal(left, negated)
}

func MultiplySignedDecimal(left, right string) (string, bool) {
	left, leftOK := NormalizeSignedDecimal(left)
	right, rightOK := NormalizeSignedDecimal(right)
	if !leftOK || !rightOK {
		return "", false
	}
	negative := strings.HasPrefix(left, "-") != strings.HasPrefix(right, "-")
	left = strings.TrimPrefix(left, "-")
	right = strings.TrimPrefix(right, "-")
	if left == "0" || right == "0" {
		return "0", true
	}
	if left == "1" || right == "1" {
		out := left
		if left == "1" {
			out = right
		}
		if negative {
			out = "-" + out
		}
		return out, true
	}
	// Canonical decimal strings longer than 20 digits cannot fit uint64.
	// Avoid reparsing those big-integer operands just to observe overflow.
	if len(left) <= 20 && len(right) <= 20 {
		if leftValue, err := strconv.ParseUint(left, 10, 64); err == nil {
			if rightValue, rightErr := strconv.ParseUint(right, 10, 64); rightErr == nil && (rightValue == 0 || leftValue <= ^uint64(0)/rightValue) {
				out := strconv.FormatUint(leftValue*rightValue, 10)
				if negative {
					out = "-" + out
				}
				return out, true
			}
		}
	}
	leftLimbs, rightLimbs := decimalLimbs(left), decimalLimbs(right)
	product := make([]uint64, len(leftLimbs)+len(rightLimbs))
	for i, a := range leftLimbs {
		carry := uint64(0)
		for j, b := range rightLimbs {
			// Each accumulated product is below 10^18 + 2*10^9.
			value := a*b + product[i+j] + carry
			product[i+j] = value % decimalLimbBase
			carry = value / decimalLimbBase
		}
		product[i+len(rightLimbs)] = carry
	}
	out := formatDecimalLimbs(product)
	if negative && out != "0" {
		out = "-" + out
	}
	return out, true
}

func DivideSignedDecimal(left, right string) (string, string, bool) {
	left, leftOK := NormalizeSignedDecimal(left)
	right, rightOK := NormalizeSignedDecimal(right)
	if !leftOK || !rightOK || right == "0" {
		return "", "", false
	}
	quotientNegative := strings.HasPrefix(left, "-") != strings.HasPrefix(right, "-")
	remainderNegative := strings.HasPrefix(left, "-")
	quotient, remainder := divideCanonicalDecimal(strings.TrimPrefix(left, "-"), strings.TrimPrefix(right, "-"))
	if quotientNegative && quotient != "0" {
		quotient = "-" + quotient
	}
	if remainderNegative && remainder != "0" {
		remainder = "-" + remainder
	}
	return quotient, remainder, true
}

func CompareUnsignedDecimal(left, right string) int {
	left, leftOK := normalizeUnsignedDecimal(left)
	right, rightOK := normalizeUnsignedDecimal(right)
	if !leftOK || !rightOK {
		return strings.Compare(left, right)
	}
	return compareCanonicalDecimal(left, right)
}

func compareCanonicalDecimal(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func AddUnsignedDecimal(left, right string) string {
	left, _ = normalizeUnsignedDecimal(left)
	right, _ = normalizeUnsignedDecimal(right)
	return addCanonicalDecimal(left, right)
}

func addCanonicalDecimal(left, right string) string {
	i, j, carry := len(left)-1, len(right)-1, 0
	var out []byte
	for i >= 0 || j >= 0 || carry != 0 {
		sum := carry
		if i >= 0 {
			sum += int(left[i] - '0')
			i--
		}
		if j >= 0 {
			sum += int(right[j] - '0')
			j--
		}
		out = append(out, byte('0'+sum%10))
		carry = sum / 10
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func SubtractUnsignedDecimal(left, right string) (string, bool) {
	left, leftOK := normalizeUnsignedDecimal(left)
	right, rightOK := normalizeUnsignedDecimal(right)
	if !leftOK || !rightOK || compareCanonicalDecimal(left, right) < 0 {
		return "", false
	}
	return subtractCanonicalDecimal(left, right), true
}

func subtractCanonicalDecimal(left, right string) string {
	i, j, borrow := len(left)-1, len(right)-1, 0
	out := make([]byte, 0, len(left))
	for i >= 0 {
		difference := int(left[i]-'0') - borrow
		if j >= 0 {
			difference -= int(right[j] - '0')
			j--
		}
		if difference < 0 {
			difference += 10
			borrow = 1
		} else {
			borrow = 0
		}
		out = append(out, byte('0'+difference))
		i--
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	first := 0
	for first < len(out)-1 && out[first] == '0' {
		first++
	}
	return string(out[first:])
}

func DivideUnsignedDecimal(left, right string) (string, string, bool) {
	left, leftOK := normalizeUnsignedDecimal(left)
	right, rightOK := normalizeUnsignedDecimal(right)
	if !leftOK || !rightOK || right == "0" {
		return "", "", false
	}
	quotient, remainder := divideCanonicalDecimal(left, right)
	return quotient, remainder, true
}

func divideCanonicalDecimal(left, right string) (string, string) {
	if right == "1" {
		return left, "0"
	}
	comparison := compareCanonicalDecimal(left, right)
	if comparison < 0 {
		return "0", left
	}
	if comparison == 0 {
		return "1", "0"
	}
	if len(left) <= 20 && len(right) <= 20 {
		if leftValue, err := strconv.ParseUint(left, 10, 64); err == nil {
			if rightValue, rightErr := strconv.ParseUint(right, 10, 64); rightErr == nil {
				return strconv.FormatUint(leftValue/rightValue, 10), strconv.FormatUint(leftValue%rightValue, 10)
			}
		}
	}
	dividend, divisor := decimalLimbs(left), decimalLimbs(right)
	quotient := make([]uint64, len(dividend)-len(divisor)+1)
	remainder := divideDecimalLimbs(dividend, divisor, quotient)
	return formatDecimalLimbs(quotient), formatDecimalLimbs(remainder)
}

// divideDecimalLimbs consumes dividend as remainder storage and restores divisor
// after normalization. A nil quotient requests only the remainder for Euclid's
// algorithm. Limbs are canonical, unsigned and least significant first.
func divideDecimalLimbs(dividend, divisor, quotient []uint64) []uint64 {
	if len(dividend) < len(divisor) {
		return dividend
	}
	if len(divisor) == 1 {
		remainder := uint64(0)
		for i := len(dividend) - 1; i >= 0; i-- {
			value := remainder*decimalLimbBase + dividend[i]
			if quotient != nil {
				quotient[i] = value / divisor[0]
			}
			remainder = value % divisor[0]
		}
		dividend[0] = remainder
		return dividend[:1]
	}
	// Normalize the leading divisor limb to at least half the base. A
	// two-limb quotient estimate then needs at most two decrements before
	// subtraction; a final add-back corrects an estimate one unit too high.
	width := len(divisor)
	quotientWidth := len(dividend) - width + 1
	scale := decimalLimbBase / (divisor[width-1] + 1)
	carry := uint64(0)
	for i, limb := range divisor {
		value := limb*scale + carry
		divisor[i] = value % decimalLimbBase
		carry = value / decimalLimbBase
	}
	carry = 0
	for i, limb := range dividend {
		value := limb*scale + carry
		dividend[i] = value % decimalLimbBase
		carry = value / decimalLimbBase
	}
	dividend = append(dividend, carry)
	for offset := quotientWidth - 1; offset >= 0; offset-- {
		leading := dividend[offset+width]*decimalLimbBase + dividend[offset+width-1]
		estimate, remainder := leading/divisor[width-1], leading%divisor[width-1]
		for estimate >= decimalLimbBase || estimate*divisor[width-2] > remainder*decimalLimbBase+dividend[offset+width-2] {
			estimate--
			remainder += divisor[width-1]
			if remainder >= decimalLimbBase {
				break
			}
		}
		borrow := uint64(0)
		for i, limb := range divisor {
			product := estimate*limb + borrow
			borrow = product / decimalLimbBase
			low := product % decimalLimbBase
			if dividend[offset+i] < low {
				dividend[offset+i] += decimalLimbBase
				borrow++
			}
			dividend[offset+i] -= low
		}
		negative := dividend[offset+width] < borrow
		dividend[offset+width] = (dividend[offset+width] + decimalLimbBase - borrow) % decimalLimbBase
		if negative {
			estimate--
			carry = 0
			for i, limb := range divisor {
				value := dividend[offset+i] + limb + carry
				dividend[offset+i] = value % decimalLimbBase
				carry = value / decimalLimbBase
			}
			dividend[offset+width] = (dividend[offset+width] + carry) % decimalLimbBase
		}
		if quotient != nil {
			quotient[offset] = estimate
		}
	}
	remainder := dividend[:width]
	carry, divisorCarry := uint64(0), uint64(0)
	for i := len(remainder) - 1; i >= 0; i-- {
		value := carry*decimalLimbBase + remainder[i]
		remainder[i] = value / scale
		carry = value % scale
		value = divisorCarry*decimalLimbBase + divisor[i]
		divisor[i] = value / scale
		divisorCarry = value % scale
	}
	for len(remainder) > 1 && remainder[len(remainder)-1] == 0 {
		remainder = remainder[:len(remainder)-1]
	}
	return remainder
}

func GCDUnsignedDecimal(left, right string) string {
	left, leftOK := normalizeUnsignedDecimal(left)
	right, rightOK := normalizeUnsignedDecimal(right)
	if !leftOK || !rightOK {
		return "1"
	}
	return gcdCanonicalDecimal(left, right)
}

func gcdCanonicalDecimal(left, right string) string {
	if right == "0" {
		return left
	}
	if right == "1" || left == "1" {
		return "1"
	}
	if len(left) <= 20 && len(right) <= 20 {
		if a, err := strconv.ParseUint(left, 10, 64); err == nil {
			if b, err := strconv.ParseUint(right, 10, 64); err == nil {
				for b != 0 {
					a, b = b, a%b
				}
				return strconv.FormatUint(a, 10)
			}
		}
	}
	dividend, divisor := decimalLimbs(left), decimalLimbs(right)
	for len(divisor) > 1 {
		remainder := divideDecimalLimbs(dividend, divisor, nil)
		dividend, divisor = divisor, remainder
	}
	a, b := divisor[0], uint64(0)
	if a == 0 {
		return formatDecimalLimbs(dividend)
	}
	for i := len(dividend) - 1; i >= 0; i-- {
		b = (b*decimalLimbBase + dividend[i]) % a
	}
	for b != 0 {
		a, b = b, a%b
	}
	return strconv.FormatUint(a, 10)
}

func Pow2UnsignedDecimal(bits int) string {
	if bits <= 0 {
		return "1"
	}
	// Base 10^9 limbs keep every product below uint64's limit while
	// advancing 29 binary digits at a time. Formatting happens only once.
	limbs := []uint64{1}
	for bits > 0 {
		shift := bits
		if shift > 29 {
			shift = 29
		}
		factor := uint64(1) << uint(shift)
		carry := uint64(0)
		for i, limb := range limbs {
			product := limb*factor + carry
			limbs[i] = product % decimalLimbBase
			carry = product / decimalLimbBase
		}
		if carry != 0 {
			limbs = append(limbs, carry)
		}
		bits -= shift
	}
	return formatDecimalLimbs(limbs)
}

const decimalLimbBase = uint64(1_000_000_000)

// decimalLimbs consumes a canonical unsigned decimal, least significant first.
func decimalLimbs(text string) []uint64 {
	limbs := make([]uint64, 0, (len(text)+8)/9)
	for end := len(text); end > 0; {
		start := end - 9
		if start < 0 {
			start = 0
		}
		var limb uint64
		for i := start; i < end; i++ {
			limb = limb*10 + uint64(text[i]-'0')
		}
		limbs = append(limbs, limb)
		end = start
	}
	return limbs
}

func formatDecimalLimbs(limbs []uint64) string {
	for len(limbs) > 1 && limbs[len(limbs)-1] == 0 {
		limbs = limbs[:len(limbs)-1]
	}
	var out strings.Builder
	out.WriteString(strconv.FormatUint(limbs[len(limbs)-1], 10))
	for i := len(limbs) - 2; i >= 0; i-- {
		part := strconv.FormatUint(limbs[i], 10)
		for padding := len(part); padding < 9; padding++ {
			out.WriteByte('0')
		}
		out.WriteString(part)
	}
	return out.String()
}

func HalveUnsignedDecimal(text string) string {
	text, ok := normalizeUnsignedDecimal(text)
	if !ok || text == "0" {
		return "0"
	}
	carry := 0
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		value := carry*10 + int(text[i]-'0')
		out = append(out, byte('0'+value/2))
		carry = value % 2
	}
	normalized, _ := normalizeUnsignedDecimal(string(out))
	return normalized
}

func BitwiseSignedDecimal(operator, left, right string) (string, bool) {
	left, leftOK := NormalizeSignedDecimal(left)
	right, rightOK := NormalizeSignedDecimal(right)
	if !leftOK || !rightOK {
		return "", false
	}
	leftMagnitude := unsignedDecimalBits(strings.TrimPrefix(left, "-"))
	rightMagnitude := unsignedDecimalBits(strings.TrimPrefix(right, "-"))
	width := len(leftMagnitude)
	if len(rightMagnitude) > width {
		width = len(rightMagnitude)
	}
	width += 2
	leftBits := signedDecimalBits(left, width)
	rightBits := signedDecimalBits(right, width)
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		switch operator {
		case "&":
			out[i] = leftBits[i] & rightBits[i]
		case "|":
			out[i] = leftBits[i] | rightBits[i]
		case "^":
			out[i] = leftBits[i] ^ rightBits[i]
		case "&^":
			out[i] = leftBits[i] & (rightBits[i] ^ 1)
		default:
			return "", false
		}
	}
	if out[width-1] == 0 {
		return unsignedBitsDecimal(out), true
	}
	twosComplementBits(out)
	magnitude := unsignedBitsDecimal(out)
	if magnitude == "0" {
		return "0", true
	}
	return "-" + magnitude, true
}

func signedDecimalBits(value string, width int) []byte {
	negative := strings.HasPrefix(value, "-")
	magnitude := unsignedDecimalBits(strings.TrimPrefix(value, "-"))
	out := make([]byte, width)
	copy(out, magnitude)
	if negative {
		twosComplementBits(out)
	}
	return out
}

func twosComplementBits(bits []byte) {
	for i := range bits {
		bits[i] ^= 1
	}
	carry := byte(1)
	for i := 0; i < len(bits) && carry != 0; i++ {
		sum := bits[i] + carry
		bits[i] = sum & 1
		carry = sum >> 1
	}
}

func unsignedDecimalBits(text string) []byte {
	text, _ = normalizeUnsignedDecimal(text)
	var out []byte
	for text != "0" {
		out = append(out, (text[len(text)-1]-'0')%2)
		text = HalveUnsignedDecimal(text)
	}
	return out
}

func unsignedBitsDecimal(bits []byte) string {
	out := "0"
	for i := len(bits) - 1; i >= 0; i-- {
		out = AddUnsignedDecimal(out, out)
		if bits[i] != 0 {
			out = AddUnsignedDecimal(out, "1")
		}
	}
	return out
}
