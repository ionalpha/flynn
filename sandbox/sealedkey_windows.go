//go:build windows

package sandbox

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// GrantSealedKeyReadable grants ALL APPLICATION PACKAGES read+execute on keyPath
// and traverse on its two parent directories, because a signer's package SID is
// new on every launch. It is safe because the key is an AES-GCM box whose
// passphrase never touches disk, and no entry is inheritable.
func GrantSealedKeyReadable(keyPath string) error {
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAnyPackageSid)
	if err != nil {
		return fmt.Errorf("sandbox: all-application-packages sid: %w", err)
	}
	signersDir := filepath.Dir(keyPath)
	dataDir := filepath.Dir(signersDir)
	// Non-inheritable, so only these three objects get the entry.
	for _, dir := range []string{dataDir, signersDir} {
		if err := grantPackagesAccess(dir, fileGenericReadExecute, sid); err != nil {
			return err
		}
	}
	return grantPackagesAccess(keyPath, fileGenericReadExecute, sid)
}

// grantPackagesAccess merges a non-inheritable entry for a well-known group,
// keeping every existing entry.
func grantPackagesAccess(path string, mask windows.ACCESS_MASK, sid *windows.SID) error {
	if err := mergeAccessEntry(path, windows.EXPLICIT_ACCESS{
		AccessPermissions: mask,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee:           sidTrustee(sid, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return nil
}
