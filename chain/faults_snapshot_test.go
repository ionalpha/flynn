package chain

// Snapshot sealer failure paths, both ends. Sealing must refuse to bind a projection to
// a log prefix it cannot vouch for, and must stop exactly at the snapshot's seq. Open
// must refuse a tampered artifact rather than restore state nobody signed.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// TestSnapshotSealerFaults is the snapshot-producer gate: a sealer refuses to bind a
// projection to a log prefix it cannot vouch for. Without a key it cannot seal at all;
// a log it cannot read, a log that does not reach the snapshot's seq, an event that
// does not canonicalize, and a signer failure all refuse rather than emit a snapshot a
// restore would later trust.
func TestSnapshotSealerFaults(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x66)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("log unavailable")

	if _, err := NewSnapshotSealer(signer, nil, nil); !hasCode(err, CodeSignerKey) {
		t.Fatalf("NewSnapshotSealer with no keyring: err = %v, want %s", err, CodeSignerKey)
	}

	t.Run("no signer", func(t *testing.T) {
		ss, err := NewSnapshotSealer(nil, ring, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ss.Seal(ctx, &scriptedLog{}, spine.Snapshot{Stream: "s", Seq: 1})
		if !hasCode(err, CodeSnapshotNoSigner) {
			t.Fatalf("Seal without a signer: err = %v, want %s", err, CodeSnapshotNoSigner)
		}
	})

	t.Run("log read fails", func(t *testing.T) {
		ss, err := NewSnapshotSealer(signer, ring, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ss.Seal(ctx, &scriptedLog{readErr: boom}, spine.Snapshot{Stream: "s", Seq: 1})
		if !errors.Is(err, boom) {
			t.Fatalf("Seal error = %v, want the log failure", err)
		}
	})

	t.Run("log does not reach the seq", func(t *testing.T) {
		ss, err := NewSnapshotSealer(signer, ring, nil)
		if err != nil {
			t.Fatal(err)
		}
		e := sampleEvent()
		e.Stream, e.Seq = "s", 1
		_, err = ss.Seal(ctx, &scriptedLog{events: []spine.Event{e}}, spine.Snapshot{Stream: "s", Seq: 9})
		if !hasCode(err, CodeSnapshotLogShort) {
			t.Fatalf("Seal past the log's tip: err = %v, want %s", err, CodeSnapshotLogShort)
		}
	})

	t.Run("event does not canonicalize", func(t *testing.T) {
		ss, err := NewSnapshotSealer(signer, ring, nil)
		if err != nil {
			t.Fatal(err)
		}
		bad := sampleEvent()
		bad.Stream, bad.Seq, bad.Time = "s", 1, time.Time{}
		_, err = ss.Seal(ctx, &scriptedLog{events: []spine.Event{bad}}, spine.Snapshot{Stream: "s", Seq: 1})
		if !hasCode(err, CodeTimeRange) {
			t.Fatalf("Seal over a non-canonical event: err = %v, want %s", err, CodeTimeRange)
		}
	})

	t.Run("signer fails", func(t *testing.T) {
		ss, err := NewSnapshotSealer(errSigner{err: boom}, ring, nil)
		if err != nil {
			t.Fatal(err)
		}
		e := sampleEvent()
		e.Stream, e.Seq = "s", 1
		_, err = ss.Seal(ctx, &scriptedLog{events: []spine.Event{e}}, spine.Snapshot{Stream: "s", Seq: 1})
		if !errors.Is(err, boom) {
			t.Fatalf("Seal error = %v, want the signer failure", err)
		}
	})
}

// TestSnapshotSealerStopsAtSeqAndUsesOrigin asserts the sealer binds the snapshot to
// exactly the prefix up to its Seq (events past it are not folded in) and scopes the
// checkpoint to the origin the caller's mapping supplies, not the raw stream id. A
// snapshot that folded in later events would attest a root the claimed prefix does not
// produce.
func TestSnapshotSealerStopsAtSeqAndUsesOrigin(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x67)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	ss, err := NewSnapshotSealer(signer, ring, func(stream string) string {
		return "flynn://instance/abc/" + stream
	})
	if err != nil {
		t.Fatal(err)
	}

	log := &scriptedLog{}
	for i := range 5 {
		e := sampleEvent()
		e.Stream, e.Seq = "s", int64(i+1)
		log.events = append(log.events, e)
	}

	sealed, err := ss.Seal(ctx, log, spine.Snapshot{Stream: "s", Seq: 3, Payload: []byte("state")})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := ss.Open(ctx, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(opened.Payload, []byte("state")) {
		t.Fatalf("opened payload = %q, want the original state", opened.Payload)
	}

	var wire snapshotWire
	if err := canonicalDec.Unmarshal(sealed.Payload, &wire); err != nil {
		t.Fatal(err)
	}
	claim, err := VerifySnapshotClaim(wire.COSE, ring)
	if err != nil {
		t.Fatalf("claim does not verify: %v", err)
	}
	if claim.Checkpoint.Size != 3 {
		t.Fatalf("signed size = %d, want 3 (the prefix up to Seq 3, not the whole log)", claim.Checkpoint.Size)
	}
	if claim.Checkpoint.Origin != "flynn://instance/abc/s" {
		t.Fatalf("origin = %q, want the mapped origin", claim.Checkpoint.Origin)
	}
}

// TestSnapshotSealerOpenRejectsTampering is the restore gate: Open must refuse anything
// the keyring does not vouch for, so a rebuild falls back to a full fold rather than
// restoring forged state. A payload that is not a sealed envelope, a claim bound to a
// different stream or seq (a replayed snapshot), and a state body swapped under a valid
// signature are all rejected.
func TestSnapshotSealerOpenRejectsTampering(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x68)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}
	ss, err := NewSnapshotSealer(signer, ring, nil)
	if err != nil {
		t.Fatal(err)
	}

	log := &scriptedLog{}
	for i := range 3 {
		e := sampleEvent()
		e.Stream, e.Seq = "s", int64(i+1)
		log.events = append(log.events, e)
	}
	sealed, err := ss.Seal(ctx, log, spine.Snapshot{Stream: "s", Seq: 3, Payload: []byte("state")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ss.Open(ctx, spine.Snapshot{Stream: "s", Seq: 3, Payload: []byte("not a sealed envelope")}); !hasCode(err, CodeSnapshotDecode) {
		t.Fatalf("Open over a raw payload: err = %v, want %s", err, CodeSnapshotDecode)
	}

	// The same sealed bytes presented under a different seq: the claim is signed but
	// bound elsewhere, so it must not restore.
	replayed := sealed
	replayed.Seq = 2
	if _, err := ss.Open(ctx, replayed); !hasCode(err, CodeSnapshotBinding) {
		t.Fatalf("Open of a snapshot replayed at another seq: err = %v, want %s", err, CodeSnapshotBinding)
	}
	replayedStream := sealed
	replayedStream.Stream = "other"
	if _, err := ss.Open(ctx, replayedStream); !hasCode(err, CodeSnapshotBinding) {
		t.Fatalf("Open of a snapshot replayed on another stream: err = %v, want %s", err, CodeSnapshotBinding)
	}

	// Swap the state body but keep the signed claim: the state hash no longer matches.
	var wire snapshotWire
	if err := canonicalDec.Unmarshal(sealed.Payload, &wire); err != nil {
		t.Fatal(err)
	}
	swapped, err := canonicalEnc.Marshal(snapshotWire{State: []byte("forged"), COSE: wire.COSE})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ss.Open(ctx, spine.Snapshot{Stream: "s", Seq: 3, Payload: swapped})
	if !hasCode(err, CodeSnapshotStateHash) {
		t.Fatalf("Open of a swapped state body: err = %v, want %s", err, CodeSnapshotStateHash)
	}

	// A checkpoint signature is not a snapshot signature: the content type is covered
	// by the signature, so replaying one as the other must fail.
	cp, err := signer.SignCheckpoint(sampleCheckpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	crossed, err := canonicalEnc.Marshal(snapshotWire{State: []byte("state"), COSE: cp.COSE})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ss.Open(ctx, spine.Snapshot{Stream: "s", Seq: 3, Payload: crossed})
	if !hasCode(err, CodeContentType) {
		t.Fatalf("Open of a checkpoint signature replayed as a snapshot: err = %v, want %s", err, CodeContentType)
	}
}
