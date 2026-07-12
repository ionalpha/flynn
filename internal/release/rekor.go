package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/transparency-dev/merkle/proof"
	"github.com/transparency-dev/merkle/rfc6962"

	"github.com/ionalpha/flynn/fault"
)

// logHasher is the RFC 6962 hashing the Rekor Merkle tree is built with. It is the
// same hasher flynn's own recording log uses, so the proof arithmetic here is the
// arithmetic the project already relies on and already tests.
var logHasher = rfc6962.DefaultHasher

// loggedEntry is the record Rekor stored, as it stored it. The verifier reads it to
// answer a question the inclusion proof alone cannot: the proof shows that *some*
// entry is in the log, and this shows that the entry is *ours*.
type loggedEntry struct {
	Spec struct {
		PayloadHash struct {
			Algorithm string `json:"algorithm"`
			Value     string `json:"value"`
		} `json:"payloadHash"`
		Signatures []struct {
			Signature string `json:"signature"`
			Verifier  string `json:"verifier"`
		} `json:"signatures"`
	} `json:"spec"`
}

// verifyInclusion proves that this exact envelope, signed by this exact certificate,
// is recorded in the transparency log this binary pins.
//
// The chain of reasoning, and it only holds if every link does: the logged entry
// commits to our payload digest, our signature, and our certificate; the entry hashes
// to a leaf; the leaf's inclusion proof reconstructs a root; and the log operator
// signed a checkpoint over that same root. An attacker who steals a Fulcio identity
// still has to get the forgery published in a log that the whole world monitors,
// which is the difference between a secret compromise and a loud one.
func (r *trustRoot) verifyInclusion(e tlogEntry, b *bundle, checkpointName string) error {
	// The bundle names the log it claims to have been recorded in. Believing that name
	// would be circular, so it is compared against the id derived from the pinned key:
	// an entry from any other log is not evidence about this one.
	if !bytes.Equal(e.keyID, r.logKeyID) {
		return fault.New(fault.Terminal, CodeTransparency,
			"the release was logged in a transparency log this binary does not trust")
	}
	if err := r.bindEntryToEnvelope(e.body, b); err != nil {
		return err
	}
	if err := r.verifyEntryTimestamp(e); err != nil {
		return err
	}

	if e.proofIndex < 0 || e.treeSize <= 0 || e.proofIndex >= e.treeSize {
		return fault.New(fault.Terminal, CodeTransparency,
			fmt.Sprintf("the inclusion proof places the entry at index %d of a %d-entry log, which is not a position that exists", e.proofIndex, e.treeSize))
	}

	leaf := logHasher.HashLeaf(e.body)
	if err := proof.VerifyInclusion(logHasher, uint64(e.proofIndex), uint64(e.treeSize), leaf, e.hashes, e.rootHash); err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency,
			fmt.Errorf("the transparency-log inclusion proof does not reconstruct the log's root: %w", err))
	}

	// The root the proof reconstructs is only meaningful if the log operator vouched
	// for it. Without this the proof proves membership in a tree the attacker made up.
	return r.verifyCheckpoint(e.checkpoint, e.rootHash, e.treeSize, checkpointName)
}

// verifyEntryTimestamp checks the log's own signature over the entry, which is what
// makes the integration time trustworthy.
//
// This matters more than it looks. The signing certificate is only valid for ten
// minutes, so the certificate chain has to be checked as of the moment the entry was
// logged, and that moment is a number that arrives inside the bundle. Left
// unauthenticated it would be an attacker-chosen input to the one check that decides
// whether an expired certificate is acceptable. The log signs it, so it is not.
func (r *trustRoot) verifyEntryTimestamp(e tlogEntry) error {
	// Rekor signs the canonical JSON of the entry's identity: its body, when it was
	// integrated, which log it went into, and where in that log it sits. The key order
	// is the canonical one (alphabetical), and the fields are exactly these four.
	signed, err := json.Marshal(struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogID          string `json:"logID"`
		LogIndex       int64  `json:"logIndex"`
	}{
		Body:           base64.StdEncoding.EncodeToString(e.body),
		IntegratedTime: e.integratedTime.Unix(),
		LogID:          hex.EncodeToString(e.keyID),
		LogIndex:       e.logIndex,
	})
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency, err)
	}

	digest := sha256.Sum256(signed)
	if !ecdsa.VerifyASN1(r.logKey, digest[:], e.promise) {
		return fault.New(fault.Terminal, CodeTransparency,
			"the transparency log did not sign this entry at the time the bundle claims it did")
	}
	return nil
}

// bindEntryToEnvelope checks that the record in the log is a record of this envelope:
// same payload digest, same signature, same certificate.
func (r *trustRoot) bindEntryToEnvelope(body []byte, b *bundle) error {
	var entry loggedEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency, fmt.Errorf("reading the logged entry: %w", err))
	}
	if entry.Spec.PayloadHash.Algorithm != "sha256" {
		return fault.New(fault.Terminal, CodeTransparency,
			"the logged entry hashes its payload with "+entry.Spec.PayloadHash.Algorithm+", which this verifier does not accept")
	}
	want := sha256.Sum256(b.payload)
	if !strings.EqualFold(entry.Spec.PayloadHash.Value, hex.EncodeToString(want[:])) {
		return fault.New(fault.Terminal, CodeTransparency,
			"the transparency-log entry records a different payload than the one in this bundle")
	}
	if len(entry.Spec.Signatures) != 1 {
		return fault.New(fault.Terminal, CodeTransparency,
			fmt.Sprintf("the transparency-log entry records %d signatures; exactly one is expected", len(entry.Spec.Signatures)))
	}
	sig, err := base64.StdEncoding.DecodeString(entry.Spec.Signatures[0].Signature)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency, fmt.Errorf("reading the logged signature: %w", err))
	}
	if !bytes.Equal(sig, b.signature) {
		return fault.New(fault.Terminal, CodeTransparency,
			"the transparency-log entry records a different signature than the one in this bundle")
	}
	verifier, err := base64.StdEncoding.DecodeString(entry.Spec.Signatures[0].Verifier)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency, fmt.Errorf("reading the logged certificate: %w", err))
	}
	if !certPEMMatches(verifier, b.leafCertificate) {
		return fault.New(fault.Terminal, CodeTransparency,
			"the transparency-log entry records a different signing certificate than the one in this bundle")
	}
	return nil
}

// verifyCheckpoint checks the log's signed statement about its own root. A checkpoint
// is a signed note: three lines of text (origin, size, root), a blank line, then one
// or more signature lines. The signature covers the text and its trailing newline,
// and nothing else.
func (r *trustRoot) verifyCheckpoint(envelope string, wantRoot []byte, wantSize int64, name string) error {
	text, sigs, ok := splitNote(envelope)
	if !ok {
		return fault.New(fault.Terminal, CodeTransparency, "the log checkpoint is not a signed note")
	}

	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 3 {
		return fault.New(fault.Terminal, CodeTransparency, "the log checkpoint is missing its origin, size, or root")
	}
	// The origin names the log and its tree. Pinning the log's name here stops a
	// checkpoint from a different (even a genuinely signed) log being replayed into
	// this position.
	origin, _, _ := strings.Cut(lines[0], " ")
	if origin != name {
		return fault.New(fault.Terminal, CodeTransparency,
			"the log checkpoint comes from "+origin+", not from "+name)
	}
	size, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency, fmt.Errorf("reading the checkpoint's tree size: %w", err))
	}
	root, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil {
		return fault.Wrap(fault.Terminal, CodeTransparency, fmt.Errorf("reading the checkpoint's root hash: %w", err))
	}
	// The signed checkpoint has to be a statement about the very tree the inclusion
	// proof was computed against, or the two halves prove nothing together.
	if size != wantSize || !bytes.Equal(root, wantRoot) {
		return fault.New(fault.Terminal, CodeTransparency,
			"the signed checkpoint describes a different tree than the inclusion proof")
	}

	for _, line := range sigs {
		signer, blob, ok := parseNoteSignature(line)
		if !ok || signer != name {
			continue
		}
		// The key hint is the first four bytes of the log's key id. It selects a key; it
		// does not authenticate anything, so the signature is still checked against the
		// key this binary pinned.
		if len(blob) <= 4 || !bytes.Equal(blob[:4], r.logKeyID[:4]) {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		if ecdsa.VerifyASN1(r.logKey, digest[:], blob[4:]) {
			return nil
		}
	}
	return fault.New(fault.Terminal, CodeTransparency,
		"the log checkpoint carries no valid signature from "+name)
}

// splitNote separates a signed note's text from its signature lines. The blank line
// between them is the separator and is not signed.
func splitNote(envelope string) (text string, sigs []string, ok bool) {
	i := strings.Index(envelope, "\n\n")
	if i < 0 {
		return "", nil, false
	}
	text = envelope[:i+1]
	rest := strings.TrimSuffix(envelope[i+2:], "\n")
	if rest == "" {
		return "", nil, false
	}
	return text, strings.Split(rest, "\n"), true
}

// parseNoteSignature reads one signature line: an em dash, the signer's name, and the
// base64 of a four-byte key hint followed by the signature.
func parseNoteSignature(line string) (signer string, blob []byte, ok bool) {
	const marker = "— "
	if !strings.HasPrefix(line, marker) {
		return "", nil, false
	}
	signer, b64, found := strings.Cut(strings.TrimPrefix(line, marker), " ")
	if !found {
		return "", nil, false
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, false
	}
	return signer, blob, true
}
