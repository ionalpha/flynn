package chain

// Producer failure paths: the Builder, the proofs a SealedRun can back, and the
// recording log's seal. The invariant: nothing emits a record or a proof it cannot
// stand behind, so an encode failure, an append failure, an empty run, or a signer
// failure all surface as errors rather than as a plausible-looking artifact.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// TestBuilderRefusesUnsealableRuns is the record-producer gate: a Builder never emits a
// record it cannot stand behind. A non-encodable event is refused at Add, an append
// failure is surfaced, an empty run is refused at Seal, a signer failure is propagated,
// and a run whose nodes cannot be snapshotted does not seal.
func TestBuilderRefusesUnsealableRuns(t *testing.T) {
	priv, _ := testKey(0x61)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("signer offline")

	t.Run("non-canonical event", func(t *testing.T) {
		b := NewBuilder("flynn://run/x")
		bad := sampleEvent()
		bad.Time = time.Time{}
		if err := b.Add(bad); !hasCode(err, CodeTimeRange) {
			t.Fatalf("Add of a zero-time event: err = %v, want %s", err, CodeTimeRange)
		}
	})

	t.Run("tree append fails", func(t *testing.T) {
		st := newErrStore()
		st.putErr = errors.New("tile write failed")
		b := &Builder{origin: "flynn://run/x", tree: NewTreeWithStore(st)}
		if err := b.Add(sampleEvent()); !errors.Is(err, st.putErr) {
			t.Fatalf("Add error = %v, want the store failure", err)
		}
	})

	t.Run("empty run", func(t *testing.T) {
		b := NewBuilder("flynn://run/x")
		if _, err := b.Seal(signer); !hasCode(err, CodeEmptyRecord) {
			t.Fatalf("Seal of an empty run: err = %v, want %s", err, CodeEmptyRecord)
		}
		if _, err := b.SealAndReset(signer); !hasCode(err, CodeEmptyRecord) {
			t.Fatalf("SealAndReset of an empty run: err = %v, want %s", err, CodeEmptyRecord)
		}
	})

	t.Run("signer fails", func(t *testing.T) {
		b := NewBuilder("flynn://run/x")
		if err := b.Add(sampleEvent()); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Seal(errSigner{err: boom}); !errors.Is(err, boom) {
			t.Fatalf("Seal error = %v, want the signer failure", err)
		}
	})

	t.Run("nodes cannot be snapshotted", func(t *testing.T) {
		b := &Builder{origin: "flynn://run/x", tree: NewTreeWithStore(bareStore{newMemNodeStore()})}
		if err := b.Add(sampleEvent()); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Seal(signer); !hasCode(err, CodeEncode) {
			t.Fatalf("Seal over a bare store: err = %v, want %s", err, CodeEncode)
		}
	})
}

// TestSealedRunEventProofFaults asserts a sealed run refuses to produce a proof it
// cannot back: an index outside the run, and a run whose retained node set cannot
// answer the inclusion path.
func TestSealedRunEventProofFaults(t *testing.T) {
	sr, _ := builtRun(t, 4)

	if _, err := sr.EventProof(4); !hasCode(err, CodeIndexRange) {
		t.Fatalf("EventProof at index == size: err = %v, want %s", err, CodeIndexRange)
	}
	if _, err := sr.EventProof(1 << 40); !hasCode(err, CodeIndexRange) {
		t.Fatalf("EventProof far past the end: err = %v, want %s", err, CodeIndexRange)
	}

	// A run whose nodes were lost cannot assemble a path, and must say so rather than
	// return a short proof that would not reconstruct the signed root.
	stripped := &SealedRun{cose: sr.cose, events: sr.events, nodes: newMemNodeStore()}
	if _, err := stripped.EventProof(0); !hasCode(err, CodeMissingNode) {
		t.Fatalf("EventProof over an emptied node store: err = %v, want %s", err, CodeMissingNode)
	}
}

// TestRecordingLogSealFaults is the recording-integrity gate: a stream whose recording
// failed must not seal, because a sealed record is supposed to cover the stream's
// complete canonical event sequence. It also asserts an unrecorded stream cannot be
// sealed, that the wrapped log's append error is propagated, and that the caller's
// origin mapping is what scopes the record.
func TestRecordingLogSealFaults(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x6d)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("append failed")

	t.Run("wrapped append error is propagated", func(t *testing.T) {
		rl := NewRecordingLog(&scriptedLog{appendErr: boom}, nil)
		if _, err := rl.Append(ctx, spine.AppendInput{Stream: "s"}); !errors.Is(err, boom) {
			t.Fatalf("Append error = %v, want the wrapped log's failure", err)
		}
	})

	t.Run("unrecorded stream", func(t *testing.T) {
		rl := NewRecordingLog(spine.NewMemoryLog(), nil)
		if _, err := rl.Seal("never-seen", signer); !hasCode(err, CodeEmptyRecord) {
			t.Fatalf("Seal of an unrecorded stream: err = %v, want %s", err, CodeEmptyRecord)
		}
		if _, err := rl.SealAndReset("never-seen", signer); !hasCode(err, CodeEmptyRecord) {
			t.Fatalf("SealAndReset of an unrecorded stream: err = %v, want %s", err, CodeEmptyRecord)
		}
	})

	t.Run("a stream whose recording failed cannot seal", func(t *testing.T) {
		// The wrapped log hands back an event the canonical encoder must refuse, so
		// recording fails while the append itself succeeds.
		bad := sampleEvent()
		bad.Stream, bad.Seq, bad.Time = "s", 1, time.Time{}
		rl := NewRecordingLog(&scriptedLog{appended: bad}, nil)

		if _, err := rl.Append(ctx, spine.AppendInput{Stream: "s"}); err != nil {
			t.Fatalf("a recording failure must not fail the append: %v", err)
		}
		if _, err := rl.Seal("s", signer); !hasCode(err, CodeTimeRange) {
			t.Fatalf("Seal after a recording failure: err = %v, want %s", err, CodeTimeRange)
		}
		if _, err := rl.SealAndReset("s", signer); !hasCode(err, CodeTimeRange) {
			t.Fatalf("SealAndReset after a recording failure: err = %v, want %s", err, CodeTimeRange)
		}
	})

	t.Run("origin mapping scopes the record", func(t *testing.T) {
		rl := NewRecordingLog(spine.NewMemoryLog(), func(stream string) string {
			return "flynn://run/" + stream
		})
		if _, err := rl.Append(ctx, spine.AppendInput{Stream: "s", Type: "t", Actor: spine.ActorSystem}); err != nil {
			t.Fatal(err)
		}
		sealed, err := rl.Seal("s", signer)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		cp, err := VerifyCheckpoint(sealed.cose, ring)
		if err != nil {
			t.Fatalf("sealed checkpoint does not verify: %v", err)
		}
		if cp.Origin != "flynn://run/s" {
			t.Fatalf("origin = %q, want the mapped origin", cp.Origin)
		}
	})
}
