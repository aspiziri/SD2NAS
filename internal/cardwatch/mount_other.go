//go:build !linux && !darwin

package cardwatch

// isMount cannot tell on this OS, so every folder counts and the DCIM/PRIVATE
// rule alone decides.
func isMount(dir, root string) (bool, error) { return true, nil }
