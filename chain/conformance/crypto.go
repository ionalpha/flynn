package conformance

// This file defines the cryptographic and governance conformance tiers (L2, L3, L4)
// that sit above the L1 structural suite in conformance.go. Where L1 fixes the
// canonical event encoding, these tiers fix the artifacts a verifier must check
// against a public key:
//
//   - L2 (checkpoint): a COSE_Sign1 signed checkpoint over a Merkle head, checked
//     with chain.VerifyCheckpoint.
//   - L3 (run, event_proof, consistency): a full signed run record checked with
//     chain.VerifyRun, a standalone single-event proof checked with
//     chain.VerifyEventProof, and a consistency proof checked with
//     chain.VerifyConsistencyProof.
//   - L4 (governance, ground_truth): a cryptographically valid run whose events carry
//     the admission lifecycle (chain.VerifyGovernance) or outcome and check records
//     (chain.VerifyGroundTruth, is a claimed success grounded in a passing check).
//
// Every artifact is produced from a fixed Ed25519 key over fixed events, and Ed25519
// signing is deterministic, so the committed golden artifacts are reproducible byte
// for byte. The suite publishes the signing key's public half (see CryptoKeyring) so
// a verifier in any language can build the same keyring and reach the same verdicts.

import (
	"crypto/ed25519"

	"github.com/fxamacker/cbor/v2"
	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// CryptoSuiteVersion identifies the cryptographic vector set; bumped with the canon.
const CryptoSuiteVersion = "0.1.0-draft"

// Kind is the artifact a cryptographic vector carries, which selects the verifier a
// conforming implementation must apply.
type Kind string

const (
	// KindCheckpoint is a COSE_Sign1 signed checkpoint, checked with VerifyCheckpoint.
	KindCheckpoint Kind = "checkpoint"
	// KindRun is a marshalled sealed run, checked with VerifyRun.
	KindRun Kind = "run"
	// KindEventProof is a marshalled single-event proof, checked with VerifyEventProof.
	KindEventProof Kind = "event_proof"
	// KindConsistency is a marshalled consistency proof between two signed checkpoints,
	// checked with VerifyConsistencyProof.
	KindConsistency Kind = "consistency"
	// KindGovernance is a marshalled sealed run whose events carry governance lifecycle
	// records, checked with VerifyRun (cryptographic) then VerifyGovernance (semantic).
	KindGovernance Kind = "governance"
	// KindGroundTruth is a marshalled sealed run whose events carry outcome and check
	// records, checked with VerifyRun then VerifyGroundTruth (is a success grounded?).
	KindGroundTruth Kind = "ground_truth"
)

// CryptoVector is one L2, L3, or L4 case: a single artifact and the verdict a
// conforming verifier must reach over it, checked against the suite keyring.
type CryptoVector struct {
	ID          string
	Tier        string
	Kind        Kind
	Expect      Verdict
	FailureCode string
	Description string
	Artifact    []byte
}

// The suite's fixed signing material. The seeds are arbitrary fixed constants, never
// derived from a clock or a random source, so the signatures and therefore the
// golden artifacts are byte-for-byte reproducible. rootKeyID is the only key the
// published keyring carries; altSeed signs the "unknown key" rejection and its public
// half is deliberately withheld from the ring.
const (
	rootKeyID = "provetrail-conformance-root"
	checkOrig = "run/conformance"
)

var (
	rootSeed = [ed25519.SeedSize]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	}
	altSeed = [ed25519.SeedSize]byte{
		32, 31, 30, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17,
		16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1,
	}
)

// cryptoEnc mirrors the canonical encoder the chain package is defined by (RFC 8949
// Core Deterministic Encoding). It is used only to re-serialize a decoded run record
// after a deliberate mutation, so a semantic-layer rejection vector (a wrong size, a
// root that does not match) is itself in canonical form and is therefore rejected by
// the intended check rather than by the container check.
var cryptoEnc = func() cbor.EncMode {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("conformance: build canonical encoder: " + err.Error())
	}
	return em
}()

// runWire mirrors the chain package's sealed-run wire layout so a valid record can be
// decoded, mutated, and re-encoded in canonical form here.
type runWire struct {
	Checkpoint []byte   `cbor:"checkpoint"`
	Events     [][]byte `cbor:"events"`
}

func mustSigner(seed [ed25519.SeedSize]byte, keyID string) *chain.Ed25519RootSigner {
	s, err := chain.NewEd25519RootSigner(keyID, ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		panic("conformance: build signer: " + err.Error())
	}
	return s
}

// RootPublicKey is the public half of the suite's root signing key, the single key a
// verifier loads into its keyring to check every accept vector. RootKeyID names it.
func RootPublicKey() ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(rootSeed[:]).Public().(ed25519.PublicKey)
}

// RootKeyID is the key id the suite's signed artifacts carry.
func RootKeyID() string { return rootKeyID }

// cryptoEvents returns n canonical events on one stream with strictly increasing Seq,
// the same shape the structural suite uses, so the cryptographic tiers are layered
// over identical events.
func cryptoEvents(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		e := baseEvent()
		e.Seq = int64(i + 1)
		out[i] = mustCanonical(e)
	}
	return out
}

// validCheckpoint signs a checkpoint over a tree built from the given events.
func validCheckpoint(signer chain.RootSigner, events [][]byte) chain.SignedCheckpoint {
	tree := chain.NewTree()
	for _, cb := range events {
		if err := tree.Append(cb); err != nil {
			panic("conformance: append leaf: " + err.Error())
		}
	}
	root, err := tree.Root()
	if err != nil {
		panic("conformance: tree root: " + err.Error())
	}
	sc, err := signer.SignCheckpoint(chain.Checkpoint{Origin: checkOrig, Size: tree.Size(), RootHash: root})
	if err != nil {
		panic("conformance: sign checkpoint: " + err.Error())
	}
	return sc
}

// signWith builds a COSE_Sign1 over payload with the given protected content type and
// key id, signed by the root key. It exists to craft checkpoint artifacts that are
// validly signed yet defective in a way the high-level signer never produces (a wrong
// content type, a payload that is not a checkpoint), so each VerifyCheckpoint failure
// path has an exact vector.
func signWith(contentType, keyID string, payload []byte) []byte {
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, ed25519.NewKeyFromSeed(rootSeed[:]))
	if err != nil {
		panic("conformance: cose signer: " + err.Error())
	}
	headers := cose.Headers{Protected: cose.ProtectedHeader{
		cose.HeaderLabelAlgorithm:   cose.AlgorithmEdDSA,
		cose.HeaderLabelContentType: contentType,
		cose.HeaderLabelKeyID:       []byte(keyID),
	}}
	msg, err := cose.Sign1(nil, signer, headers, payload, nil)
	if err != nil {
		panic("conformance: cose sign1: " + err.Error())
	}
	return msg
}

// mustMarshal panics on a marshal error from a static artifact, which can only be a
// generator bug, never a runtime condition.
func mustMarshal(b []byte, err error) []byte {
	if err != nil {
		panic("conformance: marshal artifact: " + err.Error())
	}
	return b
}

// flipLast returns a copy of b with its final byte flipped, the smallest mutation
// that invalidates a signature while leaving the surrounding structure parseable.
func flipLast(b []byte) []byte {
	out := append([]byte{}, b...)
	if len(out) > 0 {
		out[len(out)-1] ^= 0xff
	}
	return out
}

// decodeRun decodes a marshalled run into the local wire mirror for mutation.
func decodeRun(record []byte) runWire {
	var w runWire
	if err := cbor.Unmarshal(record, &w); err != nil {
		panic("conformance: decode run: " + err.Error())
	}
	return w
}

// encodeRun re-encodes a (possibly mutated) run wire in canonical form.
func encodeRun(w runWire) []byte {
	b, err := cryptoEnc.Marshal(w)
	if err != nil {
		panic("conformance: encode run: " + err.Error())
	}
	return b
}

// GenerateCrypto returns the full L2, L3, and L4 vector set, deterministically. L2
// covers every VerifyCheckpoint outcome; L3 covers every VerifyRun, VerifyEventProof,
// and VerifyConsistencyProof outcome; L4 covers the governance admission invariants
// (VerifyGovernance) and the ground-truth invariant (VerifyGroundTruth) over a
// cryptographically valid run, so each failure code has an exact vector.
func GenerateCrypto() []CryptoVector {
	root := mustSigner(rootSeed, rootKeyID)
	alt := mustSigner(altSeed, "provetrail-conformance-alt")

	events := cryptoEvents(3)
	validRun := mustMarshal(func() ([]byte, error) {
		b := chain.NewBuilder(checkOrig)
		for _, e := range threeEvents() {
			if err := b.Add(e); err != nil {
				return nil, err
			}
		}
		sealed, err := b.Seal(root)
		if err != nil {
			return nil, err
		}
		return sealed.Marshal()
	}())

	sealed := mustSealed(root)
	validProof := mustMarshal(func() ([]byte, error) {
		pf, err := sealed.EventProof(1)
		if err != nil {
			return nil, err
		}
		return pf.Marshal()
	}())

	// L2: signed checkpoints.
	good := validCheckpoint(root, events)
	l2 := []CryptoVector{
		{
			ID: "crypto.checkpoint.valid.01", Tier: "L2", Kind: KindCheckpoint, Expect: Accept,
			Description: "A COSE_Sign1 checkpoint over a three-leaf Merkle head, signed by the root key.",
			Artifact:    good.COSE,
		},
		{
			ID: "crypto.checkpoint.unknown_key.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeUnknownKey,
			Description: "A checkpoint signed by a key whose public half is not in the keyring.",
			Artifact:    validCheckpoint(alt, events).COSE,
		},
		{
			ID: "crypto.checkpoint.bad_signature.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeSignatureInvalid,
			Description: "A valid checkpoint with its final signature byte flipped.",
			Artifact:    flipLast(good.COSE),
		},
		{
			ID: "crypto.checkpoint.bad_content_type.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeContentType,
			Description: "A correctly signed COSE_Sign1 whose protected content type is not the checkpoint type.",
			Artifact:    signWith("application/provetrail-wrong", rootKeyID, mustMarshal(cryptoEnc.Marshal(map[string]any{"origin": checkOrig, "size": uint64(3), "root": []byte("x")}))),
		},
		{
			ID: "crypto.checkpoint.undecodable_payload.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeCheckpointDecode,
			Description: "A correctly signed checkpoint-typed message whose payload is not a checkpoint encoding.",
			Artifact:    signWith("application/provetrail-checkpoint+cbor", rootKeyID, []byte{0xf5}),
		},
	}

	// L3: full run records.
	runTamperRoot := func() []byte {
		w := decodeRun(validRun)
		alt := baseEvent()
		alt.Seq = 2
		alt.Payload = map[string]any{"tampered": true}
		w.Events[1] = mustCanonical(alt)
		return encodeRun(w)
	}()
	runDropEvent := func() []byte {
		w := decodeRun(validRun)
		w.Events = w.Events[:2]
		return encodeRun(w)
	}()
	runBadSig := func() []byte {
		w := decodeRun(validRun)
		w.Checkpoint = flipLast(w.Checkpoint)
		return encodeRun(w)
	}()
	runNonCanonical := mustMarshal(cryptoEnc.Marshal(map[string]any{
		"checkpoint": decodeRun(validRun).Checkpoint,
		"events":     decodeRun(validRun).Events,
		"extra":      0,
	}))

	l3run := []CryptoVector{
		{
			ID: "crypto.run.valid.01", Tier: "L3", Kind: KindRun, Expect: Accept,
			Description: "A sealed three-event run: signed checkpoint plus every event's canonical bytes.",
			Artifact:    validRun,
		},
		{
			ID: "crypto.run.root_mismatch.01", Tier: "L3", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeRootMismatch,
			Description: "A run whose events are canonical and ordered but no longer reproduce the signed root.",
			Artifact:    runTamperRoot,
		},
		{
			ID: "crypto.run.size_mismatch.01", Tier: "L3", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeSizeMismatch,
			Description: "A run with one event removed so the count no longer matches the signed size.",
			Artifact:    runDropEvent,
		},
		{
			ID: "crypto.run.bad_signature.01", Tier: "L3", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeSignatureInvalid,
			Description: "A run whose embedded checkpoint signature has been altered.",
			Artifact:    runBadSig,
		},
		{
			ID: "crypto.run.non_canonical.01", Tier: "L3", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeNonCanonical,
			Description: "A run record carrying an extra map field, so it is not in canonical form.",
			Artifact:    runNonCanonical,
		},
	}

	// L3: standalone single-event proofs.
	proofBadLeaf := func() []byte {
		pf, err := sealed.EventProof(1)
		if err != nil {
			panic("conformance: event proof: " + err.Error())
		}
		other := baseEvent()
		other.Seq = 2
		other.Payload = map[string]any{"tampered": true}
		pf.Canonical = mustCanonical(other)
		return mustMarshal(pf.Marshal())
	}()
	proofBadSize := func() []byte {
		pf, err := sealed.EventProof(1)
		if err != nil {
			panic("conformance: event proof: " + err.Error())
		}
		pf.Size = 99
		return mustMarshal(pf.Marshal())
	}()
	proofBadSig := func() []byte {
		pf, err := sealed.EventProof(1)
		if err != nil {
			panic("conformance: event proof: " + err.Error())
		}
		pf.Checkpoint = flipLast(pf.Checkpoint)
		return mustMarshal(pf.Marshal())
	}()

	l3proof := []CryptoVector{
		{
			ID: "crypto.event_proof.valid.01", Tier: "L3", Kind: KindEventProof, Expect: Accept,
			Description: "A standalone proof that the second event is included under the signed root.",
			Artifact:    validProof,
		},
		{
			ID: "crypto.event_proof.bad_inclusion.01", Tier: "L3", Kind: KindEventProof, Expect: Reject,
			FailureCode: chain.CodeInclusionInvalid,
			Description: "A proof whose event was replaced, so its leaf is not included under the signed root.",
			Artifact:    proofBadLeaf,
		},
		{
			ID: "crypto.event_proof.size_mismatch.01", Tier: "L3", Kind: KindEventProof, Expect: Reject,
			FailureCode: chain.CodeSizeMismatch,
			Description: "A proof whose claimed tree size does not match the signed size.",
			Artifact:    proofBadSize,
		},
		{
			ID: "crypto.event_proof.bad_signature.01", Tier: "L3", Kind: KindEventProof, Expect: Reject,
			FailureCode: chain.CodeSignatureInvalid,
			Description: "A proof whose embedded checkpoint signature has been altered.",
			Artifact:    proofBadSig,
		},
	}

	// L3: consistency proofs (the append-only property between two signed roots).
	validConsistency, forgedConsistency := consistencyArtifacts(root, alt)
	l3consistency := []CryptoVector{
		{
			ID: "crypto.consistency.valid.01", Tier: "L3", Kind: KindConsistency, Expect: Accept,
			Description: "A proof that the signed checkpoint over two events is a prefix of the signed checkpoint over five.",
			Artifact:    validConsistency,
		},
		{
			ID: "crypto.consistency.forged_path.01", Tier: "L3", Kind: KindConsistency, Expect: Reject,
			FailureCode: chain.CodeConsistencyInvalid,
			Description: "A consistency proof whose path does not connect the two signed roots.",
			Artifact:    forgedConsistency,
		},
		{
			ID: "crypto.consistency.unknown_key.01", Tier: "L3", Kind: KindConsistency, Expect: Reject,
			FailureCode: chain.CodeUnknownKey,
			Description: "A consistency proof whose checkpoints are signed by a key not in the keyring.",
			Artifact:    altConsistencyArtifact(alt),
		},
	}

	// L4: governance. Sealed runs whose events carry the admission lifecycle; the
	// crypto layer is valid, so only the governance semantics decide the verdict.
	l4 := []CryptoVector{
		{
			ID: "crypto.governance.valid.01", Tier: "L4", Kind: KindGovernance, Expect: Accept,
			Description: "A run where every completed action was admitted first and no denied action ran.",
			Artifact: govRun(
				root,
				govEvent(1, chain.GovStart, 1),
				govEvent(2, chain.GovEnd, 1),
				govEvent(3, chain.GovRejected, 2),
				govEvent(4, chain.GovStart, 3),
				govEvent(5, chain.GovEnd, 3),
			),
		},
		{
			ID: "crypto.governance.unadmitted_action.01", Tier: "L4", Kind: KindGovernance, Expect: Reject,
			FailureCode: chain.CodeUnadmittedAction,
			Description: "A run where an action completed with no preceding admission.",
			Artifact: govRun(
				root,
				govEvent(1, chain.GovStart, 1),
				govEvent(2, chain.GovEnd, 1),
				govEvent(3, chain.GovEnd, 2), // call 2 completes but was never admitted
			),
		},
		{
			ID: "crypto.governance.denied_but_executed.01", Tier: "L4", Kind: KindGovernance, Expect: Reject,
			FailureCode: chain.CodeDeniedButExecuted,
			Description: "A run where an action that was denied admission nonetheless completed.",
			Artifact: govRun(
				root,
				govEvent(1, chain.GovStart, 1),
				govEvent(2, chain.GovRejected, 1),
				govEvent(3, chain.GovEnd, 1), // call 1 was denied yet completes
			),
		},
	}

	// L4: ground truth. A success outcome must be grounded in a passing check.
	l4gt := []CryptoVector{
		{
			ID: "crypto.ground_truth.valid.01", Tier: "L4", Kind: KindGroundTruth, Expect: Accept,
			Description: "A run whose success outcome is grounded in a check that passed.",
			Artifact: govRun(
				root,
				gtCheck(1, 1, true),
				gtOutcome(2, chain.ResultSuccess, 1, true),
			),
		},
		{
			ID: "crypto.ground_truth.unbound_success.01", Tier: "L4", Kind: KindGroundTruth, Expect: Reject,
			FailureCode: chain.CodeNoGroundTruth,
			Description: "A run claiming success with no check bound: signed, not proven.",
			Artifact: govRun(
				root,
				gtOutcome(1, chain.ResultSuccess, 0, false),
			),
		},
		{
			ID: "crypto.ground_truth.failed_check.01", Tier: "L4", Kind: KindGroundTruth, Expect: Reject,
			FailureCode: chain.CodeNoGroundTruth,
			Description: "A run claiming success bound to a check that did not pass.",
			Artifact: govRun(
				root,
				gtCheck(1, 1, false),
				gtOutcome(2, chain.ResultSuccess, 1, true),
			),
		},
	}

	out := make([]CryptoVector, 0, len(l2)+len(l3run)+len(l3proof)+len(l3consistency)+len(l4)+len(l4gt))
	out = append(out, l2...)
	out = append(out, l3run...)
	out = append(out, l3proof...)
	out = append(out, l3consistency...)
	out = append(out, l4...)
	out = append(out, l4gt...)
	return out
}

// gtCheck builds a check-verdict event at the given stream sequence: a verification
// recorded with its own id and whether it passed.
func gtCheck(seq, id int64, passed bool) spine.Event {
	e := baseEvent()
	e.Seq = seq
	e.Type = chain.CheckRecorded
	e.Payload = map[string]any{chain.CheckRefKey: id, chain.CheckPassedKey: passed}
	return e
}

// gtOutcome builds an outcome-claim event. When bound, it names the check that grounds
// it; when not, it claims a result with no backing check.
func gtOutcome(seq int64, result string, checkID int64, bound bool) spine.Event {
	e := baseEvent()
	e.Seq = seq
	e.Type = chain.OutcomeRecorded
	p := map[string]any{chain.OutcomeResultKey: result}
	if bound {
		p[chain.CheckRefKey] = checkID
	}
	e.Payload = p
	return e
}

// govEvent builds a governance lifecycle event at the given stream sequence: a typed
// dispatch event carrying the correlation id that pairs an action's admission with
// its completion or rejection.
func govEvent(seq int64, typ string, call int64) spine.Event {
	e := baseEvent()
	e.Seq = seq
	e.Type = typ
	e.Payload = map[string]any{chain.GovCallKey: call}
	return e
}

// govRun seals a run over the given governance events, signed by signer, and returns
// the portable record. The events are sealed as-is, so a record with a deliberate
// governance defect is still cryptographically valid: only VerifyGovernance rejects it.
func govRun(signer chain.RootSigner, events ...spine.Event) []byte {
	b := chain.NewBuilder(checkOrig)
	for _, e := range events {
		if err := b.Add(e); err != nil {
			panic("conformance: add governance event: " + err.Error())
		}
	}
	sealed, err := b.Seal(signer)
	if err != nil {
		panic("conformance: seal governance run: " + err.Error())
	}
	return mustMarshal(sealed.Marshal())
}

// growingCheckpoints builds a Merkle log of five events signed by signer and returns
// the signed checkpoint at size 2, the signed checkpoint at size 5, and the
// consistency path between them.
func growingCheckpoints(signer chain.RootSigner) (before, after []byte, path [][]byte) {
	tree := chain.NewTree()
	sign := func(n uint64) []byte {
		root, err := tree.Root()
		if err != nil {
			panic("conformance: tree root: " + err.Error())
		}
		sc, err := signer.SignCheckpoint(chain.Checkpoint{Origin: checkOrig, Size: n, RootHash: root})
		if err != nil {
			panic("conformance: sign checkpoint: " + err.Error())
		}
		return sc.COSE
	}
	for i, cb := range cryptoEvents(5) {
		if err := tree.Append(cb); err != nil {
			panic("conformance: append leaf: " + err.Error())
		}
		if i+1 == 2 {
			before = sign(2)
		}
	}
	after = sign(5)
	p, err := tree.ConsistencyProof(2)
	if err != nil {
		panic("conformance: consistency proof: " + err.Error())
	}
	return before, after, p
}

// consistencyArtifacts returns a valid consistency proof signed by root, and a
// variant whose path byte is flipped so it no longer connects the two roots.
func consistencyArtifacts(root, _ chain.RootSigner) (valid, forged []byte) {
	before, after, path := growingCheckpoints(root)
	valid = mustMarshal((&chain.ConsistencyProof{Before: before, After: after, Proof: path}).Marshal())

	badPath := make([][]byte, len(path))
	copy(badPath, path)
	if len(badPath) > 0 {
		badPath[0] = flipLast(badPath[0])
	}
	forged = mustMarshal((&chain.ConsistencyProof{Before: before, After: after, Proof: badPath}).Marshal())
	return valid, forged
}

// altConsistencyArtifact returns a structurally valid consistency proof whose
// checkpoints are signed by alt, a key the published keyring does not carry.
func altConsistencyArtifact(alt chain.RootSigner) []byte {
	before, after, path := growingCheckpoints(alt)
	return mustMarshal((&chain.ConsistencyProof{Before: before, After: after, Proof: path}).Marshal())
}

// threeEvents returns the three events the run vectors are sealed over, as spine
// events so they can be added to a builder.
func threeEvents() []spine.Event {
	out := make([]spine.Event, 3)
	for i := range out {
		e := baseEvent()
		e.Seq = int64(i + 1)
		out[i] = e
	}
	return out
}

// mustSealed builds and seals the three-event run used to derive proof vectors.
func mustSealed(signer chain.RootSigner) *chain.SealedRun {
	b := chain.NewBuilder(checkOrig)
	for _, e := range threeEvents() {
		if err := b.Add(e); err != nil {
			panic("conformance: add event: " + err.Error())
		}
	}
	sealed, err := b.Seal(signer)
	if err != nil {
		panic("conformance: seal run: " + err.Error())
	}
	return sealed
}
