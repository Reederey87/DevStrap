//go:build !darwin && !linux

package daemon

// lockSocketTakeover is a no-op on platforms where the Unix daemon transport
// is not supported. Keeping the seam buildable preserves cross-platform CLI
// compilation without pretending to provide flock semantics there.
func lockSocketTakeover(_ string) (func(), error) {
	return func() {}, nil
}
