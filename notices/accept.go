package notices

import (
	"time"

	"github.com/ionalpha/flynn/fault"
)

// StaleAfter is how long a feed with no expiry stays fresh. The publisher re-signs on
// a shorter cycle, so a client that sees no fresh signature for this long is not being
// shown the current feed (a freeze attack, in The Update Framework's terms).
const StaleAfter = 7 * 24 * time.Hour

// Accept verifies doc and returns the feed with the trust state to persist. It refuses
// a document [Verify] rejects and a version below tr.Version (a rollback); the same
// version is accepted.
//
// An expired feed is accepted, because a failure to re-sign must not suppress an
// advisory. [Feed.Stale] reports it beside the content instead.
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

// Stale reports whether the feed is past its expiry. A feed with no expiry is stale
// [StaleAfter] after issue, so leaving the field out cannot make a feed immortal.
func (f Feed) Stale(now time.Time) bool {
	if !f.Expires.IsZero() {
		return now.After(f.Expires)
	}
	return now.After(f.Issued.Add(StaleAfter))
}
