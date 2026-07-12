// Package notices is the one channel that can reach an installed Flynn after it was
// installed: security advisories about Flynn itself, deprecations, and release
// notices. Without it a shipped binary is unreachable forever, and a sandbox escape
// found after a release could only be announced to the people who happened to be
// watching the repository.
//
// The channel is deliberately the opposite of the usual dynamic-config beacon. It is
// pull-only, it carries no identifier of any kind, and the document it fetches is
// byte-identical for every Flynn in the world. A server that cannot tell two clients
// apart cannot serve one of them a different answer, so a compromised or compelled
// origin cannot target a single user; the worst it can do is lie to everyone at once,
// which is what the signature stops.
//
// Three checks make the feed unforgeable and hard to suppress, and each one exists for
// an attack that a client which merely fetches and checks a signature still loses to:
//
//   - Signature. The feed is a COSE_Sign1 message over deterministic CBOR, signed
//     Ed25519 by a key in the compiled-in keyring, with the content type in the
//     protected header so a signature over a feed can never be replayed as a signature
//     over anything else. This is the construction chain/ already uses for a
//     checkpoint, for the same reason.
//   - Anti-rollback. The feed carries a monotonically increasing version. A client
//     records the highest version it has trusted and refuses anything lower, so a
//     mirror cannot replay a genuinely signed older feed to bury a fresh advisory.
//   - Anti-freeze. The feed carries a signed expiry and is re-signed on a schedule even
//     when nothing changed. Past that expiry a client treats the feed as stale and says
//     so. An attacker who blocks the origin can make Flynn go blind, but cannot make it
//     go quiet: silence is reported, never read as all-clear.
//
// A notice can say things. It can never do things. Nothing in the feed selects
// behaviour, enables a feature, opens a URL, or runs a command; the feed is text, it is
// treated as hostile text (see Sanitize), and that line is what keeps this from
// becoming a remote-control channel.
package notices
