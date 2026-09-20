package runtime

type WaitBlockedError struct {
	Message string
}

func (e WaitBlockedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "execution blocked"
}

func (vm *vm) chooseReadyIndex(ready []int) int {
	if len(ready) == 0 {
		return -1
	}
	if len(ready) == 1 || vm == nil {
		return ready[0]
	}
	state := vm.selectState
	if state == 0 {
		state = 0x9e3779b97f4a7c15
	}
	state ^= state << 13
	state ^= state >> 7
	state ^= state << 17
	vm.selectState = state
	return ready[state%uint64(len(ready))]
}
