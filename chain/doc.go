// Package chain defines the canonical wire format of a spine event and a
// structural verifier over a canonical event stream. It is the cross-language
// verifiable encoding the tamper-evident spine commits to: a deterministic CBOR
// serialization (RFC 8949 Core Deterministic Encoding) of the event envelope,
// domain-separated, so an implementation in any language can reproduce the exact
// bytes that are hashed and signed.
//
// The format is hybrid: a proof carries the exact canonical bytes of each event
// and a verifier rehashes the bytes it is given, while the canonicalization rule
// is also fully specified so a verifier can re-derive the bytes from the logical
// event and confirm they match. That removes any dependence on one language
// reproducing another's serializer while still letting a verifier check that
// carried bytes agree with their logical content.
//
// This package is the structural layer only. It validates that a stream of
// canonical event bytes is well formed, in canonical form, and strictly ordered.
// The cryptographic layer (hash chain, Merkle inclusion proofs, signed roots) is a
// separate, clearly marked slot in the verifier that is not implemented here.
// Until it is, a record verified by this package is NOT tamper-evident and must
// not be described as cryptographically verifiable.
package chain
