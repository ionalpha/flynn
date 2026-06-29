package chain

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// builtRun seals a run of n events signed by a key registered in the returned ring.
func builtRun(t *testing.T, n int) (*SealedRun, *RootKeyring) {
	t.Helper()
	priv, pub := testKey(0x10)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBuilder("flynn://run/e2e")
	for i := range n {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	sr, err := b.Seal(signer)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	return sr, ring
}

func marshalWire(t *testing.T, cose []byte, events [][]byte) []byte {
	t.Helper()
	b, err := canonicalEnc.Marshal(sealedRunWire{Checkpoint: cose, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cloneEvents(src [][]byte) [][]byte {
	out := make([][]byte, len(src))
	for i, e := range src {
		out[i] = append([]byte{}, e...)
	}
	return out
}

func TestVerifyRunRoundTrip(t *testing.T) {
	sr, ring := builtRun(t, 4)
	record, err := sr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	events, err := VerifyRun(record, ring)
	if err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has Seq %d", i, e.Seq)
		}
	}
}

func TestVerifyRunRejectsTamperedEvent(t *testing.T) {
	sr, ring := builtRun(t, 4)
	ev := cloneEvents(sr.events)
	ev[1][len(ev[1])-1] ^= 0xff
	if _, err := VerifyRun(marshalWire(t, sr.cose, ev), ring); err == nil {
		t.Fatal("verified a run with a tampered event")
	}
}

func TestVerifyRunRejectsReorder(t *testing.T) {
	sr, ring := builtRun(t, 4)
	ev := cloneEvents(sr.events)
	ev[0], ev[1] = ev[1], ev[0]
	if _, err := VerifyRun(marshalWire(t, sr.cose, ev), ring); err == nil {
		t.Fatal("verified a run with reordered events")
	}
}

func TestVerifyRunRejectsDroppedEvent(t *testing.T) {
	sr, ring := builtRun(t, 4)
	ev := cloneEvents(sr.events)[:3]
	if _, err := VerifyRun(marshalWire(t, sr.cose, ev), ring); err == nil {
		t.Fatal("verified a run with a dropped event")
	}
}

func TestVerifyRunRejectsExtraEvent(t *testing.T) {
	sr, ring := builtRun(t, 4)
	ev := cloneEvents(sr.events)
	ev = append(ev, append([]byte{}, ev[3]...))
	if _, err := VerifyRun(marshalWire(t, sr.cose, ev), ring); err == nil {
		t.Fatal("verified a run with an extra event")
	}
}

func TestVerifyRunRejectsTamperedSignature(t *testing.T) {
	sr, ring := builtRun(t, 4)
	cose := append([]byte{}, sr.cose...)
	cose[len(cose)-1] ^= 0xff
	if _, err := VerifyRun(marshalWire(t, cose, sr.events), ring); err == nil {
		t.Fatal("verified a run with a tampered checkpoint signature")
	}
}

func TestVerifyRunRejectsUnauthorizedSigner(t *testing.T) {
	sr, ring := builtRun(t, 4)
	// Re-sign the same events with a key that is not in the ring.
	rogueKey, _ := testKey(0x99)
	rogue, err := NewEd25519RootSigner("rogue", rogueKey)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBuilder("flynn://run/e2e")
	for i := range 4 {
		e := sampleEvent()
		e.Seq = int64(i + 1)
		_ = b.Add(e)
	}
	forged, err := b.Seal(rogue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRun(marshalWire(t, forged.cose, sr.events), ring); err == nil {
		t.Fatal("verified a run signed by an unauthorized key")
	}
}

func TestSealRejectsEmptyRun(t *testing.T) {
	priv, _ := testKey(0x10)
	signer, _ := NewEd25519RootSigner("inst", priv)
	if _, err := NewBuilder("o").Seal(signer); err == nil {
		t.Fatal("sealed an empty run")
	}
}

func TestEventProofRoundTrip(t *testing.T) {
	sr, ring := builtRun(t, 6)
	for i := range 6 {
		ep, err := sr.EventProof(uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		blob, err := ep.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		e, err := VerifyEventProof(blob, ring)
		if err != nil {
			t.Fatalf("valid event proof %d rejected: %v", i, err)
		}
		if e.Seq != int64(i+1) {
			t.Fatalf("event proof %d returned Seq %d", i, e.Seq)
		}
	}
}

func TestEventProofRejectsTampering(t *testing.T) {
	sr, ring := builtRun(t, 6)
	base, err := sr.EventProof(2)
	if err != nil {
		t.Fatal(err)
	}

	// Tampered event bytes.
	bad := *base
	bad.Canonical = append([]byte{}, base.Canonical...)
	bad.Canonical[len(bad.Canonical)-1] ^= 0xff
	blob, _ := bad.Marshal()
	if _, err := VerifyEventProof(blob, ring); err == nil {
		t.Fatal("verified an event proof with a tampered event")
	}

	// Wrong index.
	bad = *base
	bad.Index = 5
	blob, _ = bad.Marshal()
	if _, err := VerifyEventProof(blob, ring); err == nil {
		t.Fatal("verified an event proof with the wrong index")
	}

	// Tampered inclusion node.
	if len(base.Inclusion) > 0 {
		bad = *base
		bad.Inclusion = make([][]byte, len(base.Inclusion))
		copy(bad.Inclusion, base.Inclusion)
		bad.Inclusion[0] = flip(base.Inclusion[0])
		blob, _ = bad.Marshal()
		if _, err := VerifyEventProof(blob, ring); err == nil {
			t.Fatal("verified an event proof with a tampered inclusion node")
		}
	}

	// Tampered checkpoint signature.
	bad = *base
	bad.Checkpoint = append([]byte{}, base.Checkpoint...)
	bad.Checkpoint[len(bad.Checkpoint)-1] ^= 0xff
	blob, _ = bad.Marshal()
	if _, err := VerifyEventProof(blob, ring); err == nil {
		t.Fatal("verified an event proof with a tampered checkpoint")
	}
}

// TestPropFullPipelineNoFalseAccept is the end-to-end no-false-accept property: a
// valid run and every one of its event proofs verify, and flipping any single byte
// of the marshalled record, or of any marshalled event proof, makes verification
// fail. No mutation slips through any layer.
func TestPropFullPipelineNoFalseAccept(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 24).Draw(rt, "n")
		sr, ring := builtRun(t, n)

		record, err := sr.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		events, err := VerifyRun(record, ring)
		if err != nil {
			t.Fatalf("valid run rejected: %v", err)
		}
		if len(events) != n {
			t.Fatalf("got %d events, want %d", len(events), n)
		}

		idx := rapid.IntRange(0, n-1).Draw(rt, "proofIndex")
		ep, err := sr.EventProof(uint64(idx))
		if err != nil {
			t.Fatal(err)
		}
		proof, err := ep.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyEventProof(proof, ring); err != nil {
			t.Fatalf("valid event proof rejected: %v", err)
		}

		// Flip one byte of the record: verification must fail.
		ri := rapid.IntRange(0, len(record)-1).Draw(rt, "recordByte")
		badRecord := append([]byte{}, record...)
		badRecord[ri] ^= byte(rapid.IntRange(1, 255).Draw(rt, "recordDelta"))
		if !bytes.Equal(badRecord, record) {
			if _, err := VerifyRun(badRecord, ring); err == nil {
				t.Fatalf("a single-byte mutation of the record still verified (byte %d)", ri)
			}
		}

		// Flip one byte of the event proof: verification must fail.
		pi := rapid.IntRange(0, len(proof)-1).Draw(rt, "proofByte")
		badProof := append([]byte{}, proof...)
		badProof[pi] ^= byte(rapid.IntRange(1, 255).Draw(rt, "proofDelta"))
		if !bytes.Equal(badProof, proof) {
			if _, err := VerifyEventProof(badProof, ring); err == nil {
				t.Fatalf("a single-byte mutation of the event proof still verified (byte %d)", pi)
			}
		}
	})
}

func FuzzVerifyRunNoPanic(f *testing.F) {
	priv, pub := testKey(0x10)
	signer, _ := NewEd25519RootSigner("inst", priv)
	b := NewBuilder("o")
	e := sampleEvent()
	_ = b.Add(e)
	if sr, err := b.Seal(signer); err == nil {
		if rec, err := sr.Marshal(); err == nil {
			f.Add(rec)
		}
	}
	f.Add([]byte{})
	f.Add([]byte{0xff})
	ring := NewRootKeyring()
	_ = ring.Add("inst", pub)
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = VerifyRun(data, ring)
		_, _ = VerifyEventProof(data, ring)
	})
}
