package token

// IsIdentifierName checks the character rules for a name. Keywords are
// classified separately by the parser; reflection also uses these rules.
func IsIdentifierName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if index == 0 && !IsIdentifierStart(r) || !IsIdentifierPart(r) {
			return false
		}
	}
	return true
}

// IsExportedName reports whether the first rune belongs to Unicode category Lu.
func IsExportedName(name string) bool {
	for _, r := range name {
		return r >= 'A' && r <= 'Z' || r >= 0x80 && inUnicodeRanges(upperRanges, r)
	}
	return false
}

func inUnicodeRanges(ranges [][3]uint32, r rune) bool {
	if r < 0 {
		return false
	}
	value := uint32(r)
	low, high := 0, len(ranges)
	for low < high {
		middle := low + (high-low)/2
		row := ranges[middle]
		if value < row[0] {
			high = middle
		} else if value > row[1] {
			low = middle + 1
		} else {
			return (value-row[0])%row[2] == 0
		}
	}
	return false
}
