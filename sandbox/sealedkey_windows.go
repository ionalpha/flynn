//go:build windows

package sandbox

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// GrantSealedKeyReadable gives ALL APPLICATION PACKAGES the access a confined signer needs
// to open the sealed key the host named for it: read+execute on the key file and traverse
// on the two directories above it.
//
// A signer's AppContainer package SID is minted fresh from the scratch working directory on
// every launch (see appContainerMoniker), so an entry granted to one launch's SID is dead by
// the next. This grants the well-known ALL APPLICATION PACKAGES SID, which every AppContainer
// carries.
//
// That is safe on this file because it is the sealed key: an AES-GCM box whose passphrase
// lives only in the vault and reaches the signer over the private unlock channel, never on
// disk, so reading the ciphertext discloses nothing. The grants are scoped to read on the one
// key file and non-inheritable traverse on the parent directories, exposing no other file.
func GrantSealedKeyReadable(keyPath string) error {
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAnyPackageSid)
	if err != nil {
		return fmt.Errorf("sandbox: all-application-packages sid: %w", err)
	}
	signersDir := filepath.Dir(keyPath)
	dataDir := filepath.Dir(signersDir)
	// Traverse the parents to reach the file, then read the file itself. Non-inheritable
	// throughout, so the entry lands on exactly these three objects and nothing under them.
	for _, dir := range []string{dataDir, signersDir} {
		if err := grantPackagesAccess(dir, fileGenericReadExecute, sid); err != nil {
			return err
		}
	}
	return grantPackagesAccess(keyPath, fileGenericReadExecute, sid)
}

// grantPackagesAccess merges a non-inheritable access entry for sid into path's access
// list, keeping every existing entry. It mirrors grantDir but names an explicit mask, a
// well-known-group trustee, and no inheritance, because the sealed-key grant is a leaf
// grant on named objects rather than the working directory's inheritable write grant.
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
