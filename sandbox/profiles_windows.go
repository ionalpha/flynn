//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// profilePrefix and the digest length let the janitor match only this package's
// profiles.
const profilePrefix = "flynn.sbx."

// profileMonikerLen adds the 16 hex characters of the truncated workspace digest.
const profileMonikerLen = len(profilePrefix) + 16

// profileRoot reads LOCALAPPDATA because the folder moves with the user profile.
func profileRoot() (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", errors.New("sandbox: LOCALAPPDATA is not set; cannot locate AppContainer profiles")
	}
	return filepath.Join(local, "Packages"), nil
}

// isProfileMoniker requires the prefix, the exact length and lowercase hex, because
// deleting another application's folder under Packages destroys its state.
func isProfileMoniker(name string) bool {
	if len(name) != profileMonikerLen || !strings.HasPrefix(name, profilePrefix) {
		return false
	}
	for _, c := range name[len(profilePrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CleanStaleProfiles removes this package's profiles modified before the cutoff
// and returns the count. A profile survives a crash or a missed Close with an SID
// re-derivable from the workspace path, so its access entries stay live. Pass a
// cutoff well in the past so a live sandbox is never collected. A failed delete
// does not stop the sweep; the error joins every failure.
func CleanStaleProfiles(before time.Time) (int, error) {
	root, err := profileRoot()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // no profiles have ever been registered on this machine
		}
		return 0, fmt.Errorf("sandbox: read AppContainer profiles: %w", err)
	}

	var removed int
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !isProfileMoniker(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished under us, which is the outcome we wanted anyway
		}
		if !info.ModTime().Before(before) {
			continue // too recent to be certain no sandbox still holds it
		}
		if err := deleteAppContainerProfile(e.Name()); err != nil {
			errs = append(errs, err)
			continue
		}
		// A folder kept by an open handle is inert once the profile is gone, so its
		// removal is best-effort.
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
		removed++
	}
	return removed, errors.Join(errs...)
}
