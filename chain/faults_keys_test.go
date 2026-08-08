package chain

// Key handling and checkpoint verification. A verifier reads the key id off an artifact
// before it can check anything, so these gate the two ends of that: the id must be
// readable without trusting the signature, and the keyring must refuse an entry that
// could never verify anything. A valid signature over a payload that is not a
// checkpoint is still rejected.

import (
	"crypto/ed25519"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

// TestKeyIDExtraction asserts the key id can be read from a record and from a standalone
// checkpoint without verifying the signature (a verifier needs it to look the key up
// first), and that a malformed artifact or one with no key id is refused instead of
// yielding an empty id a keyring might match.
func TestKeyIDExtraction(t *testing.T) {
	priv, _ := testKey(0x62)
	signer, err := NewEd25519RootSigner("inst-kid", priv)
	if err != nil {
		t.Fatal(err)
	}
	sr, _ := builtRun(t, 3)
	record, err := sr.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	kid, err := RecordKeyID(record)
	if err != nil {
		t.Fatalf("RecordKeyID: %v", err)
	}
	if kid != "inst" {
		t.Fatalf("record key id = %q, want %q", kid, "inst")
	}

	sc, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	kid, err = CheckpointKeyID(sc.COSE)
	if err != nil {
		t.Fatalf("CheckpointKeyID: %v", err)
	}
	if kid != "inst-kid" {
		t.Fatalf("checkpoint key id = %q, want %q", kid, "inst-kid")
	}

	if _, err := RecordKeyID([]byte{0xff, 0xff}); !hasCode(err, CodeRecordDecode) {
		t.Fatalf("RecordKeyID over garbage: err = %v, want %s", err, CodeRecordDecode)
	}
	if _, err := CheckpointKeyID([]byte{0xff, 0xff}); !hasCode(err, CodeSignatureInvalid) {
		t.Fatalf("CheckpointKeyID over garbage: err = %v, want %s", err, CodeSignatureInvalid)
	}

	noKID := signRaw(t, priv, "", checkpointContentType, []byte{0x01})
	if _, err := CheckpointKeyID(noKID); !hasCode(err, CodeUnknownKey) {
		t.Fatalf("CheckpointKeyID with no key id: err = %v, want %s", err, CodeUnknownKey)
	}
	if _, err := RecordKeyID(marshalWire(t, noKID, [][]byte{{0x01}})); !hasCode(err, CodeUnknownKey) {
		t.Fatalf("RecordKeyID with no key id: err = %v, want %s", err, CodeUnknownKey)
	}
}

// TestKeyringRejectsUnusableKeys asserts the keyring refuses an entry that could never
// verify anything: an empty key id, or a key that is not an Ed25519 public key. Keeping
// such an entry out of the ring is what lets verification assume every registered key is
// well formed. It also asserts a signer reports the id it signs under, which is the id a
// verifier looks up.
func TestKeyringRejectsUnusableKeys(t *testing.T) {
	ring := NewRootKeyring()
	_, pub := testKey(0x63)

	if err := ring.Add("", pub); !hasCode(err, CodeSignerEmptyKeyID) {
		t.Fatalf("Add with an empty id: err = %v, want %s", err, CodeSignerEmptyKeyID)
	}
	if err := ring.Add("inst", ed25519.PublicKey{1, 2, 3}); !hasCode(err, CodeSignerKey) {
		t.Fatalf("Add with a short key: err = %v, want %s", err, CodeSignerKey)
	}
	if _, ok := ring.keys["inst"]; ok {
		t.Fatal("a refused key must not land in the ring")
	}

	priv, _ := testKey(0x64)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	if signer.KeyID() != "inst" {
		t.Fatalf("KeyID = %q, want inst", signer.KeyID())
	}
	sc, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	// The signature is valid, but its key is not in the ring: an unknown signer is
	// refused rather than trusted.
	if _, err := VerifyCheckpoint(sc.COSE, ring); !hasCode(err, CodeUnknownKey) {
		t.Fatalf("VerifyCheckpoint with an unregistered key: err = %v, want %s", err, CodeUnknownKey)
	}
	snap := signRaw(t, priv, "inst", snapshotContentType, []byte{0x01})
	if _, err := VerifySnapshotClaim(snap, ring); !hasCode(err, CodeUnknownKey) {
		t.Fatalf("VerifySnapshotClaim with an unregistered key: err = %v, want %s", err, CodeUnknownKey)
	}
}

// TestVerifyCheckpointRejectsUndecodablePayload asserts a signature that is valid over
// a payload that is not a checkpoint is still rejected. A verifier trusts the signed
// payload, so it must refuse one it cannot decode instead of returning a zero
// checkpoint that would compare equal to an empty tree.
func TestVerifyCheckpointRejectsUndecodablePayload(t *testing.T) {
	priv, pub := testKey(0x65)
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}

	// A validly signed payload under the right content type that is not CBOR at all.
	forged := signRaw(t, priv, "inst", checkpointContentType, []byte{0xff, 0xff, 0xff})
	if _, err := VerifyCheckpoint(forged, ring); !hasCode(err, CodeCheckpointDecode) {
		t.Fatalf("VerifyCheckpoint over an undecodable payload: err = %v, want %s", err, CodeCheckpointDecode)
	}

	forgedSnap := signRaw(t, priv, "inst", snapshotContentType, []byte{0xff, 0xff, 0xff})
	if _, err := VerifySnapshotClaim(forgedSnap, ring); !hasCode(err, CodeSnapshotDecode) {
		t.Fatalf("VerifySnapshotClaim over an undecodable payload: err = %v, want %s", err, CodeSnapshotDecode)
	}
}

// TestVerifyGroundTruthIgnoresCheckWithNoID asserts a check event carrying no
// correlation id grounds nothing: a success that names no check (or names one only such
// a malformed event could have registered) is still rejected, so a record cannot pass
// by recording an unaddressable check.
func TestVerifyGroundTruthIgnoresCheckWithNoID(t *testing.T) {
	check := sampleEvent()
	check.Seq, check.Type = 1, CheckRecorded
	check.Payload = map[string]any{CheckPassedKey: true} // no CheckRefKey

	outcome := sampleEvent()
	outcome.Seq, outcome.Type = 2, OutcomeRecorded
	outcome.Payload = map[string]any{OutcomeResultKey: ResultSuccess, CheckRefKey: int64(1)}

	err := VerifyGroundTruth([]spine.Event{check, outcome})
	if !hasCode(err, CodeNoGroundTruth) {
		t.Fatalf("a success bound to a check that registered no id: err = %v, want %s", err, CodeNoGroundTruth)
	}
}
