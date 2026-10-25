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
// source, so anyone can read from the repository who may speak to every installed Flynn.
//
// A rotation or a compromise ships a build; neither is a config change, so neither can be
// done to a user by writing a file on their machine.
//
// An empty ring makes the channel inert: Verify refuses every document, so nothing is
// trusted. A release must not be tagged with this map empty, since those binaries could
// never be reached afterwards.
//
// There are two keys because a binary trusts only the keys it was built with, so a single
// lost key would leave a release unreachable forever. The backup key is held offline, so
// losing the operational key costs a rotation and not the install base.
var publicKeys = map[string]string{
	// Signs the feed day to day.
	"flynn-notices-1": "746468756841a18523a1d10c87ac3402b7dec3706654904ec588115bf87ab2b4",
	// Held offline, unused, and only ever reached for if the operational key is lost or
	// compromised.
	"flynn-notices-backup-1": "97c3397962f1fd8a2141862bebba2e7b58a2b56db30907ac15fc4656a19eb945",
}

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

// SourceKeyCount reports how many keys are declared in the source. DefaultKeyring skips a
// malformed one rather than panicking, so comparing the two counts is how a typo in a key
// is caught at test time rather than on the day an advisory has to go out.
func SourceKeyCount() int { return len(publicKeys) }

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
