// Package chain defines the canonical wire format of a spine event and the
// verification layers over a canonical event stream, from structural checks up to
// independent cryptographic proof. It is the cross-language verifiable encoding the
// tamper-evident spine commits to: a deterministic CBOR serialization (RFC 8949 Core
// Deterministic Encoding) of the event envelope, domain-separated, so an
// implementation in any language can reproduce the exact bytes that are hashed and
// signed.
//
// The format is hybrid: a proof carries the exact canonical bytes of each event
// and a verifier rehashes the bytes it is given, while the canonicalization rule
// is also fully specified so a verifier can re-derive the bytes from the logical
// event and confirm they match. That removes any dependence on one language
// reproducing another's serializer while still letting a verifier check that
// carried bytes agree with their logical content.
//
// The package layers three concerns:
//
//   - Structure: Verifier.VerifyStream checks that a stream of canonical event bytes
//     is well formed, in canonical form, and strictly ordered by Seq. On its own this
//     proves shape and order, not tamper-evidence.
//   - Log: Tree is an append-only RFC 6962 Merkle log over event leaf hashes,
//     producing a head root and inclusion and consistency proofs.
//   - Signature: an Ed25519 RootSigner signs the Merkle head as a COSE_Sign1
//     checkpoint that a RootKeyring verifies, so a root is attributable and
//     revocable by key.
//
// Builder composes the three: it accumulates a run's events and seals them into a
// SealedRun, a signed checkpoint plus every event's canonical bytes. VerifyRun checks
// a sealed run end to end against a keyring: the checkpoint signature, the canonical
// form and ordering of every event, and that the events rebuild exactly the signed
// root. Passing it means an independent party trusting only the signing key can rely
// on the whole sequence being authentic and untampered. EventProof and
// VerifyEventProof give the same guarantee for a single event without the rest of the
// run.
//
// Use VerifyRun or VerifyEventProof for the cryptographic guarantee. VerifyStream is
// the structural layer they build on and is not, by itself, a tamper-evidence check.
package chain
