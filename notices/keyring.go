package notices

import (
	"encoding/hex"
	"os"
	"time"
)

// DefaultURL is the one address a Flynn ever fetches notices from. It is a constant in
// the source, not a setting: an origin a user can point somewhere else is an origin an
// attacker who can write one config file can point somewhere else, and this is the
// channel that has to survive that. Changing it means shipping a build, which is a change
// anyone can see in the diff.
const DefaultURL = "https://flynnhq.com/.well-known/flynn/notices.cose"

// RefreshInterval is how often a client looks for a new feed. It is deliberately not
// "every run": a fetch per invocation would make the origin a rough activity log of every
// user, and one check a day is already far more often than an advisory is published.
const RefreshInterval = 24 * time.Hour

// OffEnv turns the notice channel off completely: no fetch, no rendering, nothing on the
// network. It is one switch that does exactly one thing. It is deliberately not bundled
// with telemetry, error reporting, or anything else, because a switch that turns off four
// unrelated things is a switch people flip for one reason and then wonder why a fifth
// thing broke.
const OffEnv = "FLYNN_NO_NOTICES"

// publicKeys is the compiled-in keyring: the Ed25519 public keys allowed to sign a notice
// feed, keyed by the key id that appears in a feed's protected header. Hex-encoded, in
// source, so that the set of people who can say something to every installed Flynn is a
// thing anyone can read out of the repository and verify against what they are running.
//
// A rotation adds a key here and ships a build. A compromise removes one and ships a
// build. Neither is a config change, and neither can be done to a user by writing a file
// on their machine.
//
// An empty ring makes the channel inert: Verify refuses every document, so a build that
// somehow shipped without a key shows nothing at all rather than trusting anything. That
// is the correct direction to fail, but it does mean a release must not be tagged with
// this map empty, because the resulting binaries could never be reached afterwards.
var publicKeys = map[string]string{}

// DefaultKeyring builds the compiled-in keyring. A malformed entry is skipped rather than
// panicking the binary: a bad key can only ever cost us the ability to say something, and
// taking the user's agent down over it would be a self-inflicted outage.
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

// Enabled reports whether the notice channel may run in this build: the user has not set
// the off switch, and there is at least one key to trust a feed against. A keyless build
// is inert rather than credulous.
func Enabled() bool { return enabled(DefaultKeyring()) }

// enabled is the same question asked of a particular keyring, which is what a Client
// actually holds. Asking the ring in hand rather than the compiled-in one means the
// channel is exercisable end to end in a test, against a test publisher, without a test
// being able to pretend the production keyring says something it does not.
func enabled(ring *Keyring) bool {
	if v := os.Getenv(OffEnv); v != "" && v != "0" {
		return false
	}
	return ring != nil && ring.Len() > 0
}

// Due reports whether it is time to look for a new feed. A client that has never checked
// is due immediately; otherwise it waits out RefreshInterval from the last successful
// check. A Checked time in the future (a clock that was wrong, or has been set back) also
// reads as due, so a bad clock cannot park a client in a state where it never looks again.
func Due(t Trust, now time.Time) bool {
	if t.Checked.IsZero() {
		return true
	}
	if t.Checked.After(now) {
		return true
	}
	return now.Sub(t.Checked) >= RefreshInterval
}
