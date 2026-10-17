package runtime

import (
	"errors"
	"fmt"
	"math"
)

type minMaxValueKind uint8

const (
	minMaxInvalid minMaxValueKind = iota
	minMaxSigned
	minMaxUnsigned
	minMaxFloat
	minMaxString
)

func (m *moduleInstance) evalMinMax(kind minMaxValueKind, operator string, values []vmValue) (vmValue, error) {
	if len(values) == 0 {
		return vmValue{}, errors.New("min/max requires at least one value")
	}
	best := values[0]
	chooseMin := operator == "min"
	if !chooseMin && operator != "max" {
		return vmValue{}, fmt.Errorf("invalid min/max operator %q", operator)
	}
	for _, candidate := range values[1:] {
		var replace bool
		switch kind {
		case minMaxSigned:
			left, right := int64(best.scalar), int64(candidate.scalar)
			replace = chooseMin && right < left || !chooseMin && right > left
		case minMaxUnsigned:
			left, right := best.scalar, candidate.scalar
			replace = chooseMin && right < left || !chooseMin && right > left
		case minMaxFloat:
			left, right := math.Float64frombits(best.scalar), math.Float64frombits(candidate.scalar)
			// Go's min/max propagate NaN. Returning the encountered value also
			// preserves its payload and keeps the operation deterministic.
			if math.IsNaN(left) {
				continue
			}
			if math.IsNaN(right) {
				best = candidate
				continue
			}
			if left == 0 && right == 0 {
				// min(-0,+0) is -0 and max(-0,+0) is +0.
				if chooseMin {
					replace = math.Signbit(right) && !math.Signbit(left)
				} else {
					replace = !math.Signbit(right) && math.Signbit(left)
				}
			} else {
				replace = chooseMin && right < left || !chooseMin && right > left
			}
		case minMaxString:
			left, leftOK := best.payload().(string)
			right, rightOK := candidate.payload().(string)
			if !leftOK || !rightOK {
				return vmValue{}, errors.New("invalid String operands")
			}
			replace = chooseMin && right < left || !chooseMin && right > left
		default:
			return vmValue{}, fmt.Errorf("unsupported min/max type %s", best.Type)
		}
		if replace {
			best = candidate
		}
	}
	return best, nil
}
