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

// TestVerifyRecordProvenance covers the provenance tier: a record from an external
// harness run declares its tier mix, so verify reports enforced effects, unobserved
// reasoning, the harness that drove it, and that the run is non-replayable; a native
// record carries no declaration and the line is absent so its output is unchanged.
func TestVerifyRecordProvenance(t *testing.T) {
	keyID, priv := selfCertKey(t)

	external := sealedRecord(
		t, keyID, priv,
		vEvent(1, "action.dispatched", map[string]any{}),
		vEvent(2, chain.ProvenanceDeclared, map[string]any{
			chain.ProvenanceHarnessKey:    "codex",
			chain.ProvenanceEffectsKey:    chain.TierEnforced,
			chain.ProvenanceReasoningKey:  chain.TierUnobserved,
			chain.ProvenanceReplayableKey: false,
		}),
	)
	var buf bytes.Buffer
	if err := verifyRecord(&buf, "rec", external, ""); err != nil {
		t.Fatalf("an external-harness record did not verify: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"integrity:    VERIFIED", "provenance:",
		"effects ENFORCED", "reasoning UNOBSERVED", "external harness: codex", "non-replayable",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("external provenance report missing %q\n%s", want, out)
		}
	}

	// A native run carries no declaration, so the provenance line is absent.
	native := sealedRecord(t, keyID, priv, vEvent(1, "action.dispatched", map[string]any{}))
	buf.Reset()
	if err := verifyRecord(&buf, "rec", native, ""); err != nil {
		t.Fatalf("a native record did not verify: %v", err)
	}
	if strings.Contains(buf.String(), "provenance:") {
		t.Fatalf("a native run must not print a provenance line\n%s", buf.String())
	}
}

// TestVerifyRecordReportsContractDrift covers the case a sealed external record must not
// hide: the run's integrity verifies, but its harness ignored the behavioral contract it
// was given, so the same signature covers a larger unobserved surface. Verify names the
// probes that drifted and the share of tool attempts that went to the harness's own
// tools, rather than reporting a clean external run.
func TestVerifyRecordReportsContractDrift(t *testing.T) {
	keyID, priv := selfCertKey(t)

	drifted := sealedRecord(
		t, keyID, priv,
		vEvent(1, "action.dispatched", map[string]any{}),
		vEvent(2, chain.ProvenanceDeclared, map[string]any{
			chain.ProvenanceHarnessKey:    "codex",
			chain.ProvenanceEffectsKey:    chain.TierEnforced,
			chain.ProvenanceReasoningKey:  chain.TierUnobserved,
			chain.ProvenanceReplayableKey: false,
			chain.ProvenanceAttestedKey:   12,
			chain.ProvenanceNativeRateKey: 0.5,
			chain.ProvenanceDriftKey:      map[string]any{"no-native-tools": 3},
		}),
	)
	var buf bytes.Buffer
	if err := verifyRecord(&buf, "rec", drifted, ""); err != nil {
		// Drift is not an integrity failure: the record is authentic, and it is authentic
		// about the harness having ignored the contract.
		t.Fatalf("a drifted record must still verify: %v\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"integrity:    VERIFIED",
		"12 event(s) ATTESTED",
		"contract drift: no-native-tools x3",
		"50% of the harness's tool attempts used its own tools",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("drift report missing %q\n%s", want, out)
		}
	}

	// A compliant external run reports its tiers without a drift line.
	clean := sealedRecord(
		t, keyID, priv,
		vEvent(1, chain.ProvenanceDeclared, map[string]any{
			chain.ProvenanceHarnessKey:    "codex",
			chain.ProvenanceEffectsKey:    chain.TierEnforced,
			chain.ProvenanceReasoningKey:  chain.TierUnobserved,
			chain.ProvenanceReplayableKey: false,
		}),
	)
	buf.Reset()
	if err := verifyRecord(&buf, "rec", clean, ""); err != nil {
		t.Fatalf("clean external record did not verify: %v", err)
	}
	if strings.Contains(buf.String(), "drift") {
		t.Errorf("a compliant run must print no drift line\n%s", buf.String())
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
