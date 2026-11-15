// Package chain defines the canonical wire format of a spine event and the
// verification of a canonical event stream, from structural checks up to
// cryptographic proof. The encoding is deterministic CBOR (RFC 8949 Core
// Deterministic Encoding), domain-separated, so an implementation in any language
// can reproduce the exact bytes that are hashed and signed.
//
// A proof carries each event's canonical bytes and a verifier rehashes what it is
// given. The canonicalization rule is also fully specified, so a verifier can
// re-derive the bytes from the logical event and confirm they match; no language
// depends on another's serializer.
//
// Three concerns build up:
//
//   - [Verifier.VerifyStream] checks that canonical event bytes are well formed,
//     canonical and strictly ordered by Seq. That proves shape and order, not
//     tamper-evidence.
//   - [Tree] is an append-only RFC 6962 Merkle log over event leaf hashes, with
//     inclusion and consistency proofs.
//   - [Ed25519RootSigner] signs the Merkle head as a COSE_Sign1 checkpoint that a
//     [RootKeyring] verifies, so a root is attributable and revocable by key.
//
// [Builder] accumulates a run's events into a [SealedRun]. [VerifyRun] checks it
// end to end against a keyring, so a party trusting only the signing key can rely
// on the whole sequence. [EventProof] and [VerifyEventProof] do the same for one
// event. Use these, not VerifyStream alone, for a cryptographic guarantee.
package chain
