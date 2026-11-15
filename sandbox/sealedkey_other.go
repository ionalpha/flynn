//go:build !windows

package sandbox

// GrantSealedKeyReadable is a no-op, because the signer reads the key through
// ordinary file permissions the sandbox does not strip.
func GrantSealedKeyReadable(string) error { return nil }
