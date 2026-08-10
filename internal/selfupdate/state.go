package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
)

// stateFile is where the upgrade history lives, under the data directory.
const stateFile = "upgrade.json"

// state is what this machine remembers about upgrades between runs. It exists to
// defeat two attacks that no signature can catch, because in both of them every
// signature is genuine:
//
//   - Rollback: an attacker who controls what the release listing shows serves an
//     old, genuinely signed release that has a known hole in it. The signature checks
//     out, because it really was us who signed it, a year ago.
//   - Freeze: the same attacker instead shows nothing new at all, keeping a machine
//     on a version whose vulnerabilities they know, and the machine, seeing no
//     update, reports that it is up to date.
//
// Remembering the highest version ever verified, and the newest release ever seen,
// turns both of those into something this binary can notice and say out loud.
type state struct {
	// HighestVerified is the newest version whose provenance ever verified here.
	HighestVerified string `json:"highest_verified,omitempty"`
	// NewestSeen is the newest release the listing has ever offered, whether or not it
	// was installed. A listing that later offers nothing newer than this is either
	// stale or lying, and either way it is not evidence of being up to date.
	NewestSeen string `json:"newest_seen,omitempty"`
	// LastCheck is when the listing was last read, so a machine can tell "no upgrade
	// available" apart from "nobody has looked in six months".
	LastCheck time.Time `json:"last_check,omitempty"`
}

func loadState(dataDir string) state {
	// A missing or unreadable state file means no memory, not a failure: a machine that
	// refuses to upgrade because it cannot read its own notes is a machine that stays
	// unpatched. The protections it powers degrade to the current version, which is
	// still a floor.
	var s state
	// #nosec G304 -- the path is the data directory this binary was started with, joined
	// with a constant file name.
	raw, err := os.ReadFile(filepath.Join(dataDir, stateFile))
	if err != nil {
		return s
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return state{}
	}
	return s
}

func saveState(dataDir string, s state) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fault.Wrap(fault.Terminal, CodeState, err)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeState, err)
	}
	// Written through fsatomic, so an interrupted write cannot leave behind a
	// half-parsed record of what this machine has verified, and a crash right after
	// the write cannot lose it: the rename is fsynced along with the contents.
	if err := fsatomic.WriteFile(filepath.Join(dataDir, stateFile), raw, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, CodeState, err)
	}
	return nil
}

// floor is the version an upgrade must not go below: the highest of what is running
// and what was ever verified here. The running version alone is not enough, because a
// downgrade that already happened would lower the bar for the next one.
func (s state) floor(current Version) Version {
	if v, ok := ParseVersion(s.HighestVerified); ok && v.Compare(current) > 0 {
		return v
	}
	return current
}

// staleListing reports whether a release listing is older than what this machine has
// already been shown, which is the signature of a freeze or rollback attempt: the
// listing is the one thing in this whole path that is not signed, and the only defence
// against it is remembering what it said last time.
func (s state) staleListing(newest Version) (string, bool) {
	prev, ok := ParseVersion(s.NewestSeen)
	if !ok || prev.Compare(newest) <= 0 {
		return "", false
	}
	return prev.String(), true
}
