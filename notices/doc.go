// Package notices delivers security advisories, deprecations and release notices to
// an installed Flynn, so a flaw found after a release reaches the people running it.
//
// The fetch is pull-only, carries no identifier, and returns the same bytes to every
// client. An origin that cannot tell clients apart cannot target one; it can only lie
// to everyone at once, and the signature stops that. Three checks protect the feed:
//
//   - Signature: COSE_Sign1 over deterministic CBOR, Ed25519 by a compiled-in key,
//     with the content type in the protected header so a signature over another
//     document cannot be replayed as a feed.
//   - Anti-rollback: a client refuses a feed older than the newest version it has
//     trusted, so a mirror cannot replay an old signed feed to bury an advisory.
//   - Anti-freeze: the feed is re-signed on a schedule with a signed expiry. Past it
//     the client reports the feed as stale, so blocking the origin is visible and
//     never reads as all-clear.
//
// A notice is text and can never act: nothing in the feed selects behaviour, opens a
// URL or runs a command, and all text goes through [Sanitize]. The one exception is
// [Floor], which can only tighten a gate.
package notices
