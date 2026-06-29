package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/spine"
)

// vEvent builds a canonical-ready event on the test stream.
func vEvent(seq int64, typ string, payload map[string]any) spine.Event {
	return spine.Event{
		Stream:        "run/test",
		Seq:           seq,
		Time:          time.Unix(0, 1_700_000_000_000_000_000).UTC(),
		Type:          typ,
		Actor:         spine.ActorSystem,
		SchemaVersion: 1,
		Payload:       payload,
	}
}

// sealedRecord seals the given events under a signer with keyID and returns the
// portable record bytes (what `--file` reads).
func sealedRecord(t *testing.T, keyID string, priv ed25519.PrivateKey, events ...spine.Event) []byte {
	t.Helper()
	signer, err := chain.NewEd25519RootSigner(keyID, priv)
	if err != nil {
		t.Fatal(err)
	}
	b := chain.NewBuilder("run/test")
	for _, e := range events {
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	sealed, err := b.Seal(signer)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := sealed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func selfCertKey(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	return controlplane.PrincipalID(pub), priv
}

func TestVerifyRecordReportsTiers(t *testing.T) {
	keyID, priv := selfCertKey(t)
	rec := sealedRecord(t, keyID, priv, vEvent(1, "action.dispatched", map[string]any{}))

	var buf bytes.Buffer
	if err := verifyRecord(&buf, "rec", rec, ""); err != nil {
		t.Fatalf("a clean record did not pass: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"integrity:    VERIFIED", "governance:   OK", "ground-truth: not asserted"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q\n%s", want, out)
		}
	}
}

func TestVerifyRecordCatchesTamper(t *testing.T) {
	keyID, priv := selfCertKey(t)
	rec := sealedRecord(t, keyID, priv, vEvent(1, "action.dispatched", map[string]any{}))
	rec[len(rec)-1] ^= 0xff // flip a byte

	var buf bytes.Buffer
	if err := verifyRecord(&buf, "rec", rec, ""); err == nil {
		t.Fatal("a tampered record was accepted")
	}
	if !strings.Contains(buf.String(), "NOT VERIFIED") {
		t.Fatalf("tamper not reported:\n%s", buf.String())
	}
}

func TestVerifyRecordGroundTruth(t *testing.T) {
	keyID, priv := selfCertKey(t)
	run := func(passed bool) (string, error) {
		rec := sealedRecord(
			t, keyID, priv,
			vEvent(1, "action.dispatched", map[string]any{}),
			vEvent(2, chain.CheckRecorded, map[string]any{chain.CheckRefKey: int64(1), chain.CheckPassedKey: passed}),
			vEvent(3, chain.OutcomeRecorded, map[string]any{chain.OutcomeResultKey: chain.ResultSuccess, chain.CheckRefKey: int64(1)}),
		)
		var buf bytes.Buffer
		err := verifyRecord(&buf, "rec", rec, "")
		return buf.String(), err
	}

	if out, err := run(true); err != nil || !strings.Contains(out, "ground-truth: GROUNDED") {
		t.Fatalf("a grounded run was not reported grounded: err=%v\n%s", err, out)
	}
	out, err := run(false)
	if err == nil || !strings.Contains(out, "ground-truth: NOT GROUNDED") {
		t.Fatalf("a run whose check failed was reported grounded: err=%v\n%s", err, out)
	}
}

// TestVerifyRecordExternalKey covers a record signed by a key that is not a
// self-certifying principal id (a published conformance vector): it cannot be verified
// without the key, and verifies when the key is supplied in hex.
func TestVerifyRecordExternalKey(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	rec := sealedRecord(t, "conformance-root", priv, vEvent(1, "action.dispatched", map[string]any{}))

	var buf bytes.Buffer
	if err := verifyRecord(&buf, "rec", rec, ""); err == nil {
		t.Fatal("a record with a non-self-certifying key verified without --key")
	}
	buf.Reset()
	if err := verifyRecord(&buf, "rec", rec, hex.EncodeToString(pub)); err != nil {
		t.Fatalf("a record did not verify with its supplied key: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "integrity:    VERIFIED") {
		t.Fatalf("expected verification with --key:\n%s", buf.String())
	}
}
