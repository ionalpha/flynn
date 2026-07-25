//go:build !windows

package sandbox

// GrantSealedKeyReadable is a no-op off Windows. A confined signer on Linux or macOS reads
// its sealed key through ordinary POSIX file permissions (the key file is the operator's
// own, and the sandbox does not strip its read access), so there is no package SID to grant
// and nothing to do here.
func GrantSealedKeyReadable(string) error { return nil }
