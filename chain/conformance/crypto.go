package conformance

// This file defines the cryptographic and governance conformance tiers (L2, L3, L4)
// that sit above the L1 structural suite in conformance.go. Where L1 fixes the
// canonical event encoding, these tiers fix the artifacts a verifier must check
// against a public key:
//
//   - L2 (checkpoint, run): a COSE_Sign1 signed checkpoint over a Merkle head,
//     checked with chain.VerifyCheckpoint, and a full signed run record checked
//     with chain.VerifyRun. Run-record root integrity is L2 because the tier is
//     defined as Merkle-leaf integrity over carried bytes.
//   - L3 (event_proof, consistency): a standalone single-event proof checked with
//     chain.VerifyEventProof, and a consistency proof checked with
//     chain.VerifyConsistencyProof, the transparency-proof artifacts.
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
	"io"

	"github.com/fxamacker/cbor/v2"
	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/spine"
)

// CryptoSuiteVersion identifies the cryptographic vector set; bumped with the canon.
const CryptoSuiteVersion = "0.1.0"

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
	// checkpointAlg and checkpointContentType mirror the chain package's protected
	// header constants (Ed25519 as COSE -19 per RFC 9864, and the vendor-tree
	// checkpoint media type) so a defect vector differs from a valid artifact only
	// in the axis under test.
	checkpointAlg         = cose.Algorithm(-19)
	checkpointContentType = "application/vnd.provetrail.checkpoint+cbor"
)

// rawEd25519Signer is a cose.Signer carrying algorithm -19: go-cose v1.3.0 only
// dispatches the deprecated -8 label, and the crafted defect vectors need the same
// header algorithm the chain package signs with.
type rawEd25519Signer struct{ key ed25519.PrivateKey }

func (s rawEd25519Signer) Algorithm() cose.Algorithm { return checkpointAlg }

func (s rawEd25519Signer) Sign(_ io.Reader, content []byte) ([]byte, error) {
	return ed25519.Sign(s.key, content), nil
}

// spoofAlgSigner signs with Ed25519 math while claiming an arbitrary algorithm
// label, which is how the algorithm-substitution vectors are crafted: the signature
// bytes are internally consistent, and only the claimed algorithm is the defect.
type spoofAlgSigner struct {
	alg cose.Algorithm
	key ed25519.PrivateKey
}

func (s spoofAlgSigner) Algorithm() cose.Algorithm { return s.alg }

func (s spoofAlgSigner) Sign(_ io.Reader, content []byte) ([]byte, error) {
	return ed25519.Sign(s.key, content), nil
}

// signHeaders builds a COSE_Sign1 over payload with exactly the given protected
// header, signed by the root key with Ed25519 math. Unlike signWith it controls the
// whole header, so a vector can carry a substituted algorithm or omit the content
// type entirely.
func signHeaders(protected cose.ProtectedHeader, payload []byte) []byte {
	alg, err := protected.Algorithm()
	if err != nil {
		panic("conformance: protected header must carry an algorithm: " + err.Error())
	}
	msg, err := cose.Sign1(nil, spoofAlgSigner{alg: alg, key: ed25519.NewKeyFromSeed(rootSeed[:])},
		cose.Headers{Protected: protected}, payload, nil)
	if err != nil {
		panic("conformance: cose sign1: " + err.Error())
	}
	return msg
}

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
	headers := cose.Headers{Protected: cose.ProtectedHeader{
		cose.HeaderLabelAlgorithm:   checkpointAlg,
		cose.HeaderLabelContentType: contentType,
		cose.HeaderLabelKeyID:       []byte(keyID),
	}}
	msg, err := cose.Sign1(nil, rawEd25519Signer{key: ed25519.NewKeyFromSeed(rootSeed[:])}, headers, payload, nil)
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
			Artifact:    signWith(checkpointContentType, rootKeyID, []byte{0xf5}),
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
			ID: "crypto.run.valid.01", Tier: "L2", Kind: KindRun, Expect: Accept,
			Description: "A sealed three-event run: signed checkpoint plus every event's canonical bytes.",
			Artifact:    validRun,
		},
		{
			ID: "crypto.run.root_mismatch.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeRootMismatch,
			Description: "A run whose events are canonical and ordered but no longer reproduce the signed root.",
			Artifact:    runTamperRoot,
		},
		{
			ID: "crypto.run.size_mismatch.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeSizeMismatch,
			Description: "A run with one event removed so the count no longer matches the signed size.",
			Artifact:    runDropEvent,
		},
		{
			ID: "crypto.run.bad_signature.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeSignatureInvalid,
			Description: "A run whose embedded checkpoint signature has been altered.",
			Artifact:    runBadSig,
		},
		{
			ID: "crypto.run.non_canonical.01", Tier: "L2", Kind: KindRun, Expect: Reject,
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

	// L2: the checkpoint header and payload axes the mutant matrix exposed. The
	// payload of the valid three-event run's checkpoint is rebuilt here so a header
	// defect is the only fault in each vector.
	runPayload := func() []byte {
		w := decodeRun(validRun)
		tree := chain.NewTree()
		for _, cb := range w.Events {
			if err := tree.Append(cb); err != nil {
				panic("conformance: append leaf: " + err.Error())
			}
		}
		treeRoot, err := tree.Root()
		if err != nil {
			panic("conformance: tree root: " + err.Error())
		}
		return mustMarshal(cryptoEnc.Marshal(map[string]any{
			"origin": checkOrig, "size": uint64(len(w.Events)), "root": treeRoot,
		}))
	}()
	container := func(checkpoint []byte, events [][]byte) []byte {
		return encodeRun(runWire{Checkpoint: checkpoint, Events: events})
	}
	validEvents := decodeRun(validRun).Events
	kidHeader := func(alg cose.Algorithm, contentType string) cose.ProtectedHeader {
		h := cose.ProtectedHeader{
			cose.HeaderLabelAlgorithm: alg,
			cose.HeaderLabelKeyID:     []byte(rootKeyID),
		}
		if contentType != "" {
			h[cose.HeaderLabelContentType] = contentType
		}
		return h
	}

	l2header := []CryptoVector{
		{
			ID: "crypto.checkpoint.empty_tree.01", Tier: "L2", Kind: KindCheckpoint, Expect: Accept,
			Description: "A signed checkpoint over an empty tree: size 0 and the RFC 6962 empty-tree root SHA-256 of the empty string.",
			Artifact:    validCheckpoint(root, nil).COSE,
		},
		{
			ID: "crypto.checkpoint.alg_substitution.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeSignatureInvalid,
			Description: "A checkpoint whose protected algorithm claims ES256 (-7) over an Ed25519 signature: algorithm substitution must not verify.",
			Artifact:    signHeaders(kidHeader(cose.AlgorithmES256, checkpointContentType), runPayload),
		},
		{
			ID: "crypto.checkpoint.missing_content_type.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeContentType,
			Description: "A correctly signed checkpoint whose protected header omits the content type entirely.",
			Artifact:    signHeaders(kidHeader(checkpointAlg, ""), runPayload),
		},
		{
			ID: "crypto.checkpoint.payload_missing_origin.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeCheckpointDecode,
			Description: "A correctly signed checkpoint whose payload omits the origin field.",
			Artifact: signWith(checkpointContentType, rootKeyID, mustMarshal(cryptoEnc.Marshal(map[string]any{
				"size": uint64(3), "root": make([]byte, 32),
			}))),
		},
		{
			ID: "crypto.checkpoint.payload_extra_field.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeCheckpointDecode,
			Description: "A correctly signed checkpoint whose payload carries an extra field no checkpoint encoding contains.",
			Artifact: signWith(checkpointContentType, rootKeyID, mustMarshal(cryptoEnc.Marshal(map[string]any{
				"origin": checkOrig, "size": uint64(3), "root": make([]byte, 32), "extra": int64(1),
			}))),
		},
		{
			ID: "crypto.checkpoint.payload_non_canonical.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeCheckpointDecode,
			Description: "A correctly signed checkpoint whose payload map keys are not in canonical sorted order.",
			Artifact:    signWith(checkpointContentType, rootKeyID, nonCanonicalCheckpointPayload()),
		},
		{
			ID: "crypto.checkpoint.payload_short_root.01", Tier: "L2", Kind: KindCheckpoint, Expect: Reject,
			FailureCode: chain.CodeCheckpointDecode,
			Description: "A correctly signed checkpoint whose root is sixteen bytes, not a SHA-256 digest.",
			Artifact: signWith(checkpointContentType, rootKeyID, mustMarshal(cryptoEnc.Marshal(map[string]any{
				"origin": checkOrig, "size": uint64(3), "root": make([]byte, 16),
			}))),
		},
	}

	// L2: the run-container axes from the mutant differential test (m02-m19). Each
	// container carries the valid events, so the container framing or the embedded
	// checkpoint header is the only defect.
	l2run := []CryptoVector{
		{
			ID: "crypto.run.alg_substitution.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeSignatureInvalid,
			Description: "A run whose embedded checkpoint claims ES256 (-7) over an Ed25519 signature.",
			Artifact:    container(signHeaders(kidHeader(cose.AlgorithmES256, checkpointContentType), runPayload), validEvents),
		},
		{
			ID: "crypto.run.bad_content_type.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeContentType,
			Description: "A run whose embedded checkpoint carries a content type that is not the checkpoint type.",
			Artifact:    container(signHeaders(kidHeader(checkpointAlg, "application/provetrail-wrong"), runPayload), validEvents),
		},
		{
			ID: "crypto.run.missing_content_type.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeContentType,
			Description: "A run whose embedded checkpoint omits the protected content type entirely.",
			Artifact:    container(signHeaders(kidHeader(checkpointAlg, ""), runPayload), validEvents),
		},
		{
			ID: "crypto.run.unknown_key.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeUnknownKey,
			Description: "A sealed run signed by a key whose public half is not in the keyring.",
			Artifact:    mustMarshal(mustSealed(alt).Marshal()),
		},
		{
			ID: "crypto.run.duplicate_container_key.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeRecordDecode,
			Description: "A run container grown by a duplicated events entry.",
			Artifact:    dupContainerKey(validRun),
		},
		{
			ID: "crypto.run.indefinite_container.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeRecordDecode,
			Description: "A run container framed as an indefinite-length map.",
			Artifact:    indefContainer(validRun),
		},
		{
			ID: "crypto.run.trailing_bytes.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeRecordDecode,
			Description: "A valid run container followed by one extra trailing byte.",
			Artifact:    append(append([]byte{}, validRun...), 0x00),
		},
		{
			ID: "crypto.run.non_minimal_head.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeNonCanonical,
			Description: "A run container whose map head is a one-byte length argument: same structure, not the shortest encoding.",
			Artifact:    nonMinimalHeadContainer(validRun),
		},
		{
			ID: "crypto.run.events_not_bstr.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeRecordDecode,
			Description: "A run container whose events are integers rather than byte strings.",
			Artifact: mustMarshal(cryptoEnc.Marshal(map[string]any{
				"checkpoint": decodeRun(validRun).Checkpoint, "events": []any{int64(1), int64(2), int64(3)},
			})),
		},
		{
			ID: "crypto.run.empty_events.01", Tier: "L2", Kind: KindRun, Expect: Reject,
			FailureCode: chain.CodeEmptyRecord,
			Description: "A run container carrying a validly signed size-0 checkpoint and no events: a record must attest at least one event.",
			Artifact:    container(validCheckpoint(root, nil).COSE, [][]byte{}),
		},
	}

	// L3: the operational proof codes, made pinnable.
	proofIndexOOR := func() []byte {
		pf, err := sealed.EventProof(1)
		if err != nil {
			panic("conformance: event proof: " + err.Error())
		}
		pf.Index = 99
		return mustMarshal(pf.Marshal())
	}()
	proofTruncated := func() []byte {
		pf, err := sealed.EventProof(1)
		if err != nil {
			panic("conformance: event proof: " + err.Error())
		}
		if len(pf.Inclusion) == 0 {
			panic("conformance: inclusion path unexpectedly empty")
		}
		pf.Inclusion = pf.Inclusion[:len(pf.Inclusion)-1]
		return mustMarshal(pf.Marshal())
	}()
	l3ops := []CryptoVector{
		{
			ID: "crypto.event_proof.index_out_of_range.01", Tier: "L3", Kind: KindEventProof, Expect: Reject,
			FailureCode: chain.CodeIndexRange,
			Description: "A proof whose claimed index lies outside the signed tree size.",
			Artifact:    proofIndexOOR,
		},
		{
			ID: "crypto.event_proof.missing_node.01", Tier: "L3", Kind: KindEventProof, Expect: Reject,
			FailureCode: chain.CodeMissingNode,
			Description: "A proof whose inclusion path is one node short of what the tree shape requires.",
			Artifact:    proofTruncated,
		},
	}

	out := make([]CryptoVector, 0, len(l2)+len(l2header)+len(l3run)+len(l2run)+len(l3proof)+len(l3ops)+len(l3consistency)+len(l4)+len(l4gt))
	out = append(out, l2...)
	out = append(out, l2header...)
	out = append(out, l3run...)
	out = append(out, l2run...)
	out = append(out, l3proof...)
	out = append(out, l3ops...)
	out = append(out, l3consistency...)
	out = append(out, l4...)
	out = append(out, l4gt...)
	return out
}

// nonCanonicalCheckpointPayload encodes the checkpoint fields in declaration order
// (origin, size, root) without key sorting; canonical order is root, size, origin,
// so these bytes decode to a checkpoint but are not its canonical encoding.
func nonCanonicalCheckpointPayload() []byte {
	w := struct {
		Origin string `cbor:"origin"`
		Size   uint64 `cbor:"size"`
		Root   []byte `cbor:"root"`
	}{Origin: checkOrig, Size: 3, Root: make([]byte, 32)}
	b, err := nonSortEnc.Marshal(w)
	if err != nil {
		panic("conformance: encode non-canonical checkpoint payload: " + err.Error())
	}
	return b
}

// dupContainerKey grows a valid run container by a duplicated events entry.
func dupContainerKey(record []byte) []byte {
	if record[0] != 0xA2 {
		panic("conformance: run container does not start as a two-entry map")
	}
	out := append([]byte{}, record...)
	out[0] = 0xA3
	return append(out, 0x66, 'e', 'v', 'e', 'n', 't', 's', 0x80)
}

// indefContainer re-frames a valid run container as an indefinite-length map.
func indefContainer(record []byte) []byte {
	if record[0] != 0xA2 {
		panic("conformance: run container does not start as a two-entry map")
	}
	out := append([]byte{0xBF}, record[1:]...)
	return append(out, 0xFF)
}

// nonMinimalHeadContainer re-frames a valid run container's map head as a one-byte
// length argument (0xB8 0x02): same structure, not the shortest encoding.
func nonMinimalHeadContainer(record []byte) []byte {
	if record[0] != 0xA2 {
		panic("conformance: run container does not start as a two-entry map")
	}
	return append([]byte{0xB8, 0x02}, record[1:]...)
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
