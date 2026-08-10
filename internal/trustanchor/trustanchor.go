// Package trustanchor holds the pinned Sigstore trust anchors, and is the only place
// they are stored.
//
// Two packages verify signatures against them: internal/release checks flynn's own
// releases, and internal/sigstore checks extension releases. Both need the same Fulcio
// chain, and neither imports the other, so the chain was previously committed twice
// under two names. That made a rotation into a trap. Updating one file left the other
// verification path pinned to a retired CA, and every test in both packages stayed
// green, because each package only ever saw its own copy. One embedded copy removes
// the condition rather than detecting it.
//
// The anchors are compiled in rather than fetched, because a trust root downloaded at
// verification time is only as trustworthy as the connection it came over, which is
// the thing release verification defends against. Rotating them means shipping a
// release, which is the same act a user already has to trust.
package trustanchor

import "embed"

// Files carries both anchors under the trust/ prefix, for a caller that wants to load
// them the way the binary will: from a filesystem it can substitute in a test.
//
//go:embed trust/fulcio.pem trust/rekor.pub
var Files embed.FS

// Fulcio is Sigstore's public-good Fulcio CA chain, the root plus its intermediate,
// as PEM.
//
//go:embed trust/fulcio.pem
var Fulcio []byte
