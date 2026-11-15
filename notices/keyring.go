package notices

import (
	"encoding/hex"
	"os"
	"time"
)

// DefaultURL is the only notice origin. It is a constant so an attacker who can write
// a config file cannot redirect the channel; changing it ships a build.
const DefaultURL = "https://flynnhq.com/.well-known/flynn/notices.cose"

// RefreshInterval is how often a client looks for a new feed. A fetch per run would
// turn the origin's logs into an activity log of every user.
const RefreshInterval = 24 * time.Hour

// OffEnv turns the notice channel off when set to anything but "" or "0": no fetch, no
// rendering. It controls nothing else.
const OffEnv = "FLYNN_NO_NOTICES"

// publicKeys holds the hex Ed25519 keys allowed to sign a feed, by key id. Rotation
// ships a build. A release must not be tagged with this map empty, since an empty ring
// verifies nothing and those binaries could never be reached.
//
// A binary trusts only the keys it was built with, so the backup key, held offline,
// keeps a lost operational key from leaving every release unreachable.
var publicKeys = map[string]string{
	// Signs the feed day to day.
	"flynn-notices-1": "746468756841a18523a1d10c87ac3402b7dec3706654904ec588115bf87ab2b4",
	// Held offline; used only if the operational key is lost or compromised.
	"flynn-notices-backup-1": "97c3397962f1fd8a2141862bebba2e7b58a2b56db30907ac15fc4656a19eb945",
}

// DefaultKeyring builds the compiled-in keyring, skipping a malformed entry so a bad
// key cannot crash the binary.
func DefaultKeyring() *Keyring {
	ring := NewKeyring()
	for id, h := range publicKeys {
		pub, err := hex.DecodeString(h)
		if err != nil {
			continue
		}
		_ = ring.Add(id, pub)
	}
	return ring
}

// SourceKeyCount reports how many keys the source declares. A test compares it with
// [DefaultKeyring]'s length to catch a typo in a key.
func SourceKeyCount() int { return len(publicKeys) }

// Enabled reports whether [OffEnv] is unset and the compiled-in ring has a key.
func Enabled() bool { return enabled(DefaultKeyring()) }

// enabled asks of the Client's own ring, so a test can drive the channel against a
// test publisher without altering the production keyring.
func enabled(ring *Keyring) bool {
	if v := os.Getenv(OffEnv); v != "" && v != "0" {
		return false
	}
	return ring != nil && ring.Len() > 0
}

// Due reports whether [RefreshInterval] has passed since t.Checked. A zero or future
// Checked is due, so a clock set back cannot stop a client from ever checking.
func Due(t Trust, now time.Time) bool {
	if t.Checked.IsZero() {
		return true
	}
	if t.Checked.After(now) {
		return true
	}
	return now.Sub(t.Checked) >= RefreshInterval
}
