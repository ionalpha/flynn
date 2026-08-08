package chain

// Durable recorder failure paths. Recording is best-effort on the event path and fails
// closed everywhere else: a per-event record routes its failure to the handler, while an
// explicit Checkpoint, a reload, and a shutdown sweep all report their errors to the
// caller.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// TestDurableRecorderRoutesFailuresToHandler is the best-effort-recording gate: the
// wrapped append is the source of truth, so a failure in the integrity layer must reach
// the error handler and still return the event, never fail the append. It also asserts
// the wrapped log's own append error is propagated unchanged, since that one is real.
func TestDurableRecorderRoutesFailuresToHandler(t *testing.T) {
	ctx := context.Background()
	priv, _ := testKey(0x69)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("checkpoint store is down")

	t.Run("wrapped append error is propagated", func(t *testing.T) {
		rec := NewDurableRecorder(&scriptedLog{appendErr: boom}, func(string) FlushNodeStore {
			return newErrStore()
		}, newFakeCheckpointStore(), signer, nil, 1)
		if _, err := rec.Append(ctx, spine.AppendInput{Stream: "s", Type: "t", Actor: spine.ActorSystem}); !errors.Is(err, boom) {
			t.Fatalf("Append error = %v, want the wrapped log's failure", err)
		}
	})

	t.Run("recording failure reaches the handler", func(t *testing.T) {
		ckpts := &errCheckpointStore{fakeCheckpointStore: newFakeCheckpointStore(), loadErr: boom}
		var got []error
		rec := NewDurableRecorder(spine.NewMemoryLog(), func(string) FlushNodeStore {
			return newErrStore()
		}, ckpts, signer, nil, 1).OnError(func(err error) { got = append(got, err) })

		e, err := rec.Append(ctx, spine.AppendInput{Stream: "s", Type: "t", Actor: spine.ActorSystem})
		if err != nil {
			t.Fatalf("Append must not fail on a recording error: %v", err)
		}
		if e.Seq != 1 {
			t.Fatalf("event Seq = %d, want the wrapped append's result", e.Seq)
		}
		if len(got) != 1 || !errors.Is(got[0], boom) {
			t.Fatalf("handler saw %v, want the checkpoint store failure", got)
		}
	})

	t.Run("recording failure with no handler is dropped", func(t *testing.T) {
		ckpts := &errCheckpointStore{fakeCheckpointStore: newFakeCheckpointStore(), loadErr: boom}
		rec := NewDurableRecorder(spine.NewMemoryLog(), func(string) FlushNodeStore {
			return newErrStore()
		}, ckpts, signer, nil, 1)
		if _, err := rec.Append(ctx, spine.AppendInput{Stream: "s", Type: "t", Actor: spine.ActorSystem}); err != nil {
			t.Fatalf("Append must not fail with no handler set: %v", err)
		}
	})
}

// TestDurableRecorderCheckpointFaults asserts an explicit Checkpoint, unlike a
// best-effort recording, surfaces its error: the caller sealing a stream's tip must
// learn that the tiles did not flush, the head could not be signed, or the checkpoint
// did not persist, rather than believe the stream is durably attested when it is not.
func TestDurableRecorderCheckpointFaults(t *testing.T) {
	ctx := context.Background()
	priv, _ := testKey(0x6a)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}

	newRec := func(t *testing.T, store FlushNodeStore, ckpts CheckpointStore, sg RootSigner) *DurableRecorder {
		t.Helper()
		log := spine.NewMemoryLog()
		rec := NewDurableRecorder(log, func(string) FlushNodeStore { return store }, ckpts, sg, nil, 0)
		appendDurable(t, rec, "s", 3)
		return rec
	}

	t.Run("flush fails", func(t *testing.T) {
		st := newErrStore()
		st.flushErr = errors.New("tiles did not page out")
		rec := newRec(t, st, newFakeCheckpointStore(), signer)
		if err := rec.Checkpoint(ctx, "s"); !errors.Is(err, st.flushErr) {
			t.Fatalf("Checkpoint error = %v, want the flush failure", err)
		}
	})

	t.Run("signing fails", func(t *testing.T) {
		boom := errors.New("key unavailable")
		rec := newRec(t, newErrStore(), newFakeCheckpointStore(), errSigner{err: boom})
		if err := rec.Checkpoint(ctx, "s"); !errors.Is(err, boom) {
			t.Fatalf("Checkpoint error = %v, want the signer failure", err)
		}
	})

	t.Run("persist fails", func(t *testing.T) {
		boom := errors.New("write failed")
		ckpts := &errCheckpointStore{fakeCheckpointStore: newFakeCheckpointStore(), saveErr: boom}
		rec := newRec(t, newErrStore(), ckpts, signer)
		if err := rec.Checkpoint(ctx, "s"); !errors.Is(err, boom) {
			t.Fatalf("Checkpoint error = %v, want the persist failure", err)
		}
	})

	t.Run("load fails", func(t *testing.T) {
		boom := errors.New("read failed")
		ckpts := &errCheckpointStore{fakeCheckpointStore: newFakeCheckpointStore(), loadErr: boom}
		rec := NewDurableRecorder(spine.NewMemoryLog(), func(string) FlushNodeStore { return newErrStore() },
			ckpts, signer, nil, 0)
		if err := rec.Checkpoint(ctx, "s"); !errors.Is(err, boom) {
			t.Fatalf("Checkpoint error = %v, want the checkpoint read failure", err)
		}
	})

	t.Run("empty stream has nothing to seal", func(t *testing.T) {
		ckpts := newFakeCheckpointStore()
		rec := NewDurableRecorder(spine.NewMemoryLog(), func(string) FlushNodeStore { return newErrStore() },
			ckpts, signer, nil, 0)
		if err := rec.Checkpoint(ctx, "untouched"); err != nil {
			t.Fatalf("Checkpoint of an empty stream: %v", err)
		}
		if _, _, ok, _ := ckpts.LatestCheckpoint(ctx, "untouched"); ok {
			t.Fatal("an empty stream must not produce a checkpoint")
		}
	})
}

// TestDurableRecorderEnsureLoadedFaults asserts the reload path fails closed. A
// checkpoint that claims a size the tiles cannot back, a log the recorder cannot read,
// and a stored event that does not canonicalize must all be reported rather than
// resumed on a tree that silently diverges from the signed history.
func TestDurableRecorderEnsureLoadedFaults(t *testing.T) {
	ctx := context.Background()
	priv, _ := testKey(0x6b)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("checkpoint size the tiles cannot back", func(t *testing.T) {
		ckpts := newFakeCheckpointStore()
		if err := ckpts.SaveCheckpoint(ctx, "s", 4, []byte("cose")); err != nil {
			t.Fatal(err)
		}
		rec := NewDurableRecorder(spine.NewMemoryLog(), func(string) FlushNodeStore { return newErrStore() },
			ckpts, signer, nil, 0)
		if err := rec.Checkpoint(ctx, "s"); !hasCode(err, CodeMissingNode) {
			t.Fatalf("reload over empty tiles: err = %v, want %s", err, CodeMissingNode)
		}
	})

	t.Run("log read fails", func(t *testing.T) {
		boom := errors.New("log read failed")
		rec := NewDurableRecorder(&scriptedLog{readErr: boom}, func(string) FlushNodeStore { return newErrStore() },
			newFakeCheckpointStore(), signer, nil, 0)
		if err := rec.Checkpoint(ctx, "s"); !errors.Is(err, boom) {
			t.Fatalf("Checkpoint error = %v, want the log read failure", err)
		}
	})

	t.Run("stored event does not canonicalize", func(t *testing.T) {
		bad := sampleEvent()
		bad.Stream, bad.Seq, bad.Time = "s", 1, time.Time{}
		rec := NewDurableRecorder(&scriptedLog{events: []spine.Event{bad}},
			func(string) FlushNodeStore { return newErrStore() }, newFakeCheckpointStore(), signer, nil, 0)
		if err := rec.Checkpoint(ctx, "s"); !hasCode(err, CodeTimeRange) {
			t.Fatalf("reload over a non-canonical event: err = %v, want %s", err, CodeTimeRange)
		}
	})

	t.Run("tile write fails during the re-fold", func(t *testing.T) {
		good := sampleEvent()
		good.Stream, good.Seq = "s", 1
		st := newErrStore()
		st.putErr = errors.New("tile write failed")
		rec := NewDurableRecorder(&scriptedLog{events: []spine.Event{good}},
			func(string) FlushNodeStore { return st }, newFakeCheckpointStore(), signer, nil, 0)
		if err := rec.Checkpoint(ctx, "s"); !errors.Is(err, st.putErr) {
			t.Fatalf("Checkpoint error = %v, want the tile write failure", err)
		}
	})
}

// TestDurableRecorderCheckpointAll asserts a shutdown sweep seals every stream it
// recorded, each head verifying against an independent fold of the log, and that one
// stream's failure neither skips the others nor is swallowed.
func TestDurableRecorderCheckpointAll(t *testing.T) {
	ctx := context.Background()
	priv, pub := testKey(0x6c)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	ring := NewRootKeyring()
	if err := ring.Add("inst", pub); err != nil {
		t.Fatal(err)
	}

	log := spine.NewMemoryLog()
	stores := map[string]*durableFlushStore{}
	nodes := func(s string) FlushNodeStore {
		st, ok := stores[s]
		if !ok {
			st = &durableFlushStore{newTiledNodeStore()}
			stores[s] = st
		}
		return st
	}
	ckpts := newFakeCheckpointStore()
	// A non-nil origin mapping, so the sweep's checkpoints carry the mapped origin.
	rec := NewDurableRecorder(log, nodes, ckpts, signer, func(s string) string {
		return "flynn://instance/abc/" + s
	}, 0)

	streams := []string{"a", "b", "c"}
	for _, s := range streams {
		appendDurable(t, rec, s, 5)
	}
	if _, _, ok, _ := ckpts.LatestCheckpoint(ctx, "a"); ok {
		t.Fatal("a recorder with no cadence must not checkpoint on its own")
	}

	if err := rec.CheckpointAll(ctx); err != nil {
		t.Fatalf("CheckpointAll: %v", err)
	}
	for _, s := range streams {
		size, cose, ok, err := ckpts.LatestCheckpoint(ctx, s)
		if err != nil || !ok {
			t.Fatalf("no checkpoint for %s (err=%v)", s, err)
		}
		if size != 5 {
			t.Fatalf("stream %s sealed at size %d, want 5", s, size)
		}
		cp, err := VerifyCheckpoint(cose, ring)
		if err != nil {
			t.Fatalf("checkpoint %s does not verify: %v", s, err)
		}
		if cp.Origin != "flynn://instance/abc/"+s {
			t.Fatalf("origin = %q, want the mapped origin", cp.Origin)
		}
		if want := referenceRoot(t, log, s); !bytes.Equal(cp.RootHash, want) {
			t.Fatalf("stream %s head does not match the independent log fold", s)
		}
	}

	// One stream's persist failure must surface, and the sweep must still visit the
	// rest: every stream is asked to save again.
	boom := errors.New("write failed")
	failing := &errCheckpointStore{fakeCheckpointStore: ckpts, saveErr: boom}
	rec2 := NewDurableRecorder(log, nodes, failing, signer, nil, 0)
	for _, s := range streams {
		appendDurable(t, rec2, s, 1)
	}
	if err := rec2.CheckpointAll(ctx); !errors.Is(err, boom) {
		t.Fatalf("CheckpointAll error = %v, want the persist failure", err)
	}
}

// TestDurableRecorderRecordPathFaults asserts the per-event record path (as opposed to
// the reload path) reports its failures: an event the canonical encoder refuses, and a
// tile write that fails. Both must reach the error handler rather than leave the tree
// silently out of step with the log.
func TestDurableRecorderRecordPathFaults(t *testing.T) {
	ctx := context.Background()
	priv, _ := testKey(0x6e)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("appended event does not canonicalize", func(t *testing.T) {
		bad := sampleEvent()
		bad.Stream, bad.Seq, bad.Time = "s", 1, time.Time{}
		var got []error
		rec := NewDurableRecorder(&scriptedLog{appended: bad},
			func(string) FlushNodeStore { return newErrStore() }, newFakeCheckpointStore(),
			signer, nil, 1).OnError(func(err error) { got = append(got, err) })

		if _, err := rec.Append(ctx, spine.AppendInput{Stream: "s"}); err != nil {
			t.Fatalf("a recording failure must not fail the append: %v", err)
		}
		if len(got) != 1 || !hasCode(got[0], CodeTimeRange) {
			t.Fatalf("handler saw %v, want a %s fault", got, CodeTimeRange)
		}
	})

	t.Run("tile write fails", func(t *testing.T) {
		good := sampleEvent()
		good.Stream, good.Seq = "s", 1
		st := newErrStore()
		st.putErr = errors.New("tile write failed")
		var got []error
		rec := NewDurableRecorder(&scriptedLog{appended: good},
			func(string) FlushNodeStore { return st }, newFakeCheckpointStore(),
			signer, nil, 1).OnError(func(err error) { got = append(got, err) })

		if _, err := rec.Append(ctx, spine.AppendInput{Stream: "s"}); err != nil {
			t.Fatalf("a recording failure must not fail the append: %v", err)
		}
		if len(got) != 1 || !errors.Is(got[0], st.putErr) {
			t.Fatalf("handler saw %v, want the tile write failure", got)
		}
	})

	t.Run("CheckpointAll surfaces a stream that cannot load", func(t *testing.T) {
		boom := errors.New("checkpoint read failed")
		ckpts := &errCheckpointStore{fakeCheckpointStore: newFakeCheckpointStore(), loadErr: boom}
		rec := NewDurableRecorder(spine.NewMemoryLog(),
			func(string) FlushNodeStore { return newErrStore() }, ckpts, signer, nil, 0).
			OnError(func(error) {})
		appendDurable(t, rec, "s", 2)
		if err := rec.CheckpointAll(ctx); !errors.Is(err, boom) {
			t.Fatalf("CheckpointAll error = %v, want the load failure", err)
		}
	})
}

// TestDurableRecorderEvictionDisabled asserts a non-positive residency cap keeps every
// touched stream resident, which is the documented way to opt out of eviction.
func TestDurableRecorderEvictionDisabled(t *testing.T) {
	priv, _ := testKey(0x6f)
	signer, err := NewEd25519RootSigner("inst", priv)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewDurableRecorder(spine.NewMemoryLog(), func(string) FlushNodeStore {
		return &durableFlushStore{newTiledNodeStore()}
	}, newFakeCheckpointStore(), signer, nil, 0).WithMaxResident(0)

	for _, s := range []string{"a", "b", "c", "d"} {
		appendDurable(t, rec, s, 2)
	}
	if got := len(rec.streams); got != 4 {
		t.Fatalf("resident streams = %d, want all 4 kept with eviction disabled", got)
	}
}
