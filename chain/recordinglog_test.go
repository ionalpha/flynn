package chain

import (
	"context"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/spine"
)

func appendN(t *testing.T, log spine.Log, stream string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		if _, err := log.Append(ctx, spine.AppendInput{
			Stream:  stream,
			Type:    "action.dispatched",
			Actor:   spine.ActorAgent,
			Payload: map[string]any{"i": int64(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordingLogSealAndVerify(t *testing.T) {
	rl := NewRecordingLog(spine.NewMemoryLog(), nil)
	appendN(t, rl, "run/x", 5)

	priv, pub := testKey(0x20)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := rl.Seal("run/x", signer)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	record, err := sr.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	events, err := VerifyRun(record, ring)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("verified %d events, want 5", len(events))
	}

	// The wrapped log still holds the events: recording does not interfere.
	got, err := rl.Read(context.Background(), spine.Query{Stream: "run/x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("wrapped log holds %d events, want 5", len(got))
	}

	// A tampered record fails to verify.
	bad := append([]byte{}, record...)
	bad[len(bad)/2] ^= 0xff
	if _, err := VerifyRun(bad, ring); err == nil {
		t.Fatal("verified a tampered record")
	}
}

func TestRecordingLogSealIsImmutableSnapshot(t *testing.T) {
	rl := NewRecordingLog(spine.NewMemoryLog(), nil)
	appendN(t, rl, "run/x", 3)
	priv, pub := testKey(0x21)
	signer, _ := NewEd25519RootSigner("inst", priv)
	sr, err := rl.Seal("run/x", signer)
	if err != nil {
		t.Fatal(err)
	}
	record, _ := sr.Marshal()

	// More events arrive after sealing. The earlier record must still verify, and at
	// its original size, because a sealed record is a snapshot.
	appendN(t, rl, "run/x", 2)
	ring := NewRootKeyring()
	_ = ring.Add("inst", pub)
	events, err := VerifyRun(record, ring)
	if err != nil {
		t.Fatalf("snapshot record failed to verify after later appends: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("snapshot grew to %d events", len(events))
	}
}

func TestRecordingLogSealUnrecordedStream(t *testing.T) {
	rl := NewRecordingLog(spine.NewMemoryLog(), nil)
	priv, _ := testKey(0x22)
	signer, _ := NewEd25519RootSigner("inst", priv)
	if _, err := rl.Seal("never-recorded", signer); err == nil {
		t.Fatal("sealed an unrecorded stream")
	}
}

func TestPropRecordingLogRoundTrip(t *testing.T) {
	priv, pub := testKey(0x23)
	signer, _ := NewEd25519RootSigner("inst", priv)
	ring := NewRootKeyring()
	_ = ring.Add("inst", pub)
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 20).Draw(rt, "n")
		rl := NewRecordingLog(spine.NewMemoryLog(), nil)
		appendN(t, rl, "run/x", n)
		sr, err := rl.Seal("run/x", signer)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		record, err := sr.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		events, err := VerifyRun(record, ring)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if len(events) != n {
			t.Fatalf("verified %d events, want %d", len(events), n)
		}
	})
}

// TestRecordingLogSealAndReset rotates a recorded stream: the first segment seals
// and verifies, and appends after the reset accumulate into a second independently
// verifiable segment with continuing Seq numbers.
func TestRecordingLogSealAndReset(t *testing.T) {
	rl := NewRecordingLog(spine.NewMemoryLog(), nil)
	appendN(t, rl, "run/x", 3)

	priv, pub := testKey(0x20)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}

	first, err := rl.SealAndReset("run/x", signer)
	if err != nil {
		t.Fatalf("seal and reset: %v", err)
	}
	appendN(t, rl, "run/x", 3)
	second, err := rl.Seal("run/x", signer)
	if err != nil {
		t.Fatalf("seal after rotation: %v", err)
	}

	wantSeq := int64(1)
	for _, sr := range []*SealedRun{first, second} {
		record, err := sr.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		events, err := VerifyRun(record, ring)
		if err != nil {
			t.Fatalf("rotated segment rejected: %v", err)
		}
		if len(events) != 3 {
			t.Fatalf("segment holds %d events, want 3", len(events))
		}
		if events[0].Seq != wantSeq {
			t.Fatalf("segment starts at Seq %d, want %d", events[0].Seq, wantSeq)
		}
		wantSeq += 3
	}
}
