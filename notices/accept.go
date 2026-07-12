package notices

import (
	"time"

	"github.com/ionalpha/flynn/fault"
)

// StaleAfter is how long a feed may go without a fresh signature before a client says it
// has lost sight of the channel. The publisher re-signs on a shorter cycle than this even
// when nothing has changed, exactly so that a client which stops seeing fresh signatures
// has learned something true: it is not being shown the current feed. (The Update
// Framework calls the attack this defends against a freeze attack, and its answer is the
// same: sign a short-lived statement on a schedule, so an attacker who blocks the origin
// cannot hold a client at an old view indefinitely without the client noticing.)
const StaleAfter = 7 * 24 * time.Hour

// Accept verifies a fetched feed document against the keyring and the client's trust
// state, and returns the feed together with the trust state to persist.
//
// It refuses exactly two things, and both of them mean forgery or replay rather than bad
// luck:
//
//   - a document that does not verify against a key in the ring, or is not a feed at all
//     (Verify's checks), and
//   - a feed older than the newest one this client has already trusted. That is the
//     rollback defence: an origin or a mirror that serves a genuinely signed but stale
//     feed is trying to bury a newer advisory, and the monotonic version is what makes
//     that visible. Re-serving the same version is fine and normal.
//
// It deliberately does NOT refuse an expired feed, and that is a decision worth being
// explicit about. Expiry tells the client the channel has gone quiet; it says nothing
// about whether the notices inside are true. If our re-signing job broke on the same
// weekend we published a critical advisory, a client that refused the expired feed would
// refuse the advisory, which is precisely backwards: the failure of our infrastructure
// would suppress the warning it exists to deliver. So staleness never suppresses content.
// It surfaces as a warning next to the content (see Feed.Stale), which is the honest
// thing to show and the thing an attacker who is blocking the origin cannot prevent.
func Accept(doc []byte, ring *Keyring, tr Trust, now time.Time) (Feed, Trust, error) {
	f, err := Verify(doc, ring)
	if err != nil {
		return Feed{}, tr, err
	}
	if f.Version < tr.Version {
		return Feed{}, tr, fault.New(fault.Terminal, CodeRollback,
			"notices: feed is older than the newest one already trusted, which means it is being rolled back")
	}
	tr.Version = f.Version
	tr.Checked = now.UTC()
	return f, tr, nil
}

// Stale reports whether the feed has gone too long without a fresh signature, and so
// whether the client should tell the user it may not be seeing current notices. A feed
// whose expiry has passed is stale by definition; a feed with no expiry set is held to
// StaleAfter from when it was issued, so a publisher cannot make a feed immortal by
// leaving the field out.
func (f Feed) Stale(now time.Time) bool {
	if !f.Expires.IsZero() {
		return now.After(f.Expires)
	}
	return now.After(f.Issued.Add(StaleAfter))
}
