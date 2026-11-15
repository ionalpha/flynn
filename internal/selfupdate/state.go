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

// state is what this machine remembers about upgrades between runs. It defeats two
// attacks no signature catches, because every signature in them is genuine:
//
//   - Rollback: an attacker controlling the release listing serves an old, genuinely
//     signed release with a known hole.
//   - Freeze: the attacker shows nothing new, keeping the machine on a version whose
//     vulnerabilities they know while it reports itself up to date.
//
// Remembering the highest version verified and the newest release seen lets this
// binary notice both and say so.
type state struct {
	// HighestVerified is the newest version whose provenance ever verified here.
	HighestVerified string `json:"highest_verified,omitempty"`
	// NewestSeen is the newest release the listing ever offered, installed or not. A
	// listing that later offers nothing newer is stale or lying, and either way is no
	// evidence of being up to date.
	NewestSeen string `json:"newest_seen,omitempty"`
	// LastCheck is when the listing was last read, so "no upgrade available" is told
	// apart from "nobody has looked in six months".
	LastCheck time.Time `json:"last_check,omitempty"`
}

func loadState(dataDir string) state {
	// A missing or unreadable state file means no memory, and no failure: refusing to
	// upgrade over unreadable notes would leave the machine unpatched. The protections
	// fall back to the current version, which is still a floor.
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
	// fsatomic means an interrupted write cannot leave a half-written record of what
	// was verified, and a crash just after cannot lose it, since the rename is fsynced
	// with the contents.
	if err := fsatomic.WriteFile(filepath.Join(dataDir, stateFile), raw, 0o600); err != nil {
		return fault.Wrap(fault.Terminal, CodeState, err)
	}
	return nil
}

// floor is the version an upgrade must not go below: the higher of what runs and what
// was ever verified here. The running version alone would let a past downgrade lower
// the bar for the next one.
func (s state) floor(current Version) Version {
	if v, ok := ParseVersion(s.HighestVerified); ok && v.Compare(current) > 0 {
		return v
	}
	return current
}

// staleListing reports whether a listing is older than what this machine was already
// shown, the sign of a freeze or rollback attempt. The listing is the one unsigned
// input on this path, and remembering what it said last time is the only defence.
func (s state) staleListing(newest Version) (string, bool) {
	prev, ok := ParseVersion(s.NewestSeen)
	if !ok || prev.Compare(newest) <= 0 {
		return "", false
	}
	return prev.String(), true
}
