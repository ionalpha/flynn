// Package trustanchor holds the pinned Sigstore trust anchors, and is the only place
// they are stored. internal/release (flynn's own releases) and internal/sigstore
// (extension releases) both verify against the same Fulcio chain without importing
// each other. With two copies, a rotation that updated one would leave the other path
// pinned to a retired CA while every test stayed green, since each package sees only
// its own copy.
//
// The anchors are compiled in because a root downloaded at verification time is only
// as trustworthy as its connection, which is what release verification defends
// against. Rotating them means shipping a release.
package trustanchor

import "embed"

// Files carries both anchors under the trust/ prefix, for a caller that loads them from
// a filesystem a test can substitute.
//
//go:embed trust/fulcio.pem trust/rekor.pub
var Files embed.FS

// Fulcio is Sigstore's public-good Fulcio CA chain, the root plus its intermediate,
// as PEM.
//
//go:embed trust/fulcio.pem
var Fulcio []byte
