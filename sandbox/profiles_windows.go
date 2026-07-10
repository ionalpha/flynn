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

// profilePrefix is the moniker prefix every AppContainer profile this package registers
// carries (see appContainerMoniker). The janitor matches on it, and on the fixed length
// of the digest that follows, so it can never touch a profile another program registered.
const profilePrefix = "flynn.sbx."

// profileMonikerLen is the length of a moniker: the prefix plus the 16 hex characters of
// the truncated workspace digest.
const profileMonikerLen = len(profilePrefix) + 16

// profileRoot is the directory Windows keeps registered AppContainer profile folders in,
// one per moniker. It is derived from the environment rather than a constant because it
// moves with the user profile.
func profileRoot() (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", errors.New("sandbox: LOCALAPPDATA is not set; cannot locate AppContainer profiles")
	}
	return filepath.Join(local, "Packages"), nil
}

// isProfileMoniker reports whether a directory name is one of this package's AppContainer
// profiles: the exact prefix, the exact total length, and nothing but hex digits after the
// prefix. A name that merely starts with the prefix is not enough, because deleting a
// directory under Packages that this package did not create would destroy another
// application's container state.
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

// CleanStaleProfiles removes the AppContainer profiles this package registered whose
// folders were last modified before the given cutoff, and reports how many it removed.
//
// It exists because a profile outlives its sandbox whenever Close does not run: a crashed
// run, a killed process, a caller that forgot. Each survivor keeps a registered container
// identity whose SID is re-derivable from the workspace path, so an access entry granted
// to it stays live rather than becoming dead, and its folder accumulates on disk without
// bound. A machine that has been running confined commands for a while therefore needs a
// collector, not just a correct teardown.
//
// The cutoff is what makes this safe to run while other sandboxes are alive: a profile in
// use belongs to a process that registered it recently, so a caller passes a cutoff well
// in the past (an hour, a day) and a live run is never collected. The caller supplies the
// cutoff rather than this package reading the clock, because wall time enters the codebase
// only through the clock port.
//
// Removal is not all-or-nothing. A profile that cannot be deleted (in use, or a permission
// the process does not hold) is counted as an error and the sweep continues, so one stuck
// profile does not strand the rest. The returned error joins every failure.
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
		// Deleting the profile unregisters the identity and normally takes the folder with
		// it. A folder left behind (an open handle held it) is inert once the profile is
		// gone, so its removal is best-effort and its failure is not an error.
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
		removed++
	}
	return removed, errors.Join(errs...)
}
