package runtime

import "testing"

func TestSelectionUsesAllReadyCases(t *testing.T) {
	machine := &vm{}
	seen := map[int]bool{}
	for range 32 {
		seen[machine.chooseReadyIndex([]int{0, 1})] = true
	}
	if !seen[0] || !seen[1] {
		t.Fatalf("ready selections = %#v, want both cases", seen)
	}
}
