package runtime

// preparedField binds a declared field to its physical layout once. The
// instruction evaluates at most 16 steps and holds no lock across steps.
type preparedField struct {
	index    int
	indirect bool
	typ      vmType
}
