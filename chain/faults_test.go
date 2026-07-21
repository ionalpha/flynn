package chain

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/spine"
)

// errStore fails a NodeStore operation on demand, so the tests can drive the storage
// failure paths a Tree, a Builder, and a durable recorder must surface rather than
// silently produce an unbacked proof.
type errStore struct {
	inner NodeStore

	putErr   error
	putLevel int // -1 puts fail at any level; otherwise only at this level
	getErr   error
	missAll  bool // Node always reports absent
	flushErr error
}

func newErrStore() *errStore {
	return &errStore{inner: newMemNodeStore(), putLevel: -1}
}

func (s *errStore) Node(level uint, index uint64) ([]byte, bool, error) {
	if s.getErr != nil {
		return nil, false, s.getErr
	}
	if s.missAll {
		return nil, false, nil
	}
	return s.inner.Node(level, index)
}

func (s *errStore) PutNode(level uint, index uint64, hash []byte) error {
	if s.putErr != nil && (s.putLevel < 0 || uint(s.putLevel) == level) {
		return s.putErr
	}
	return s.inner.PutNode(level, index, hash)
}

func (s *errStore) Flush() error { return s.flushErr }

// bareStore is a NodeStore that cannot snapshot itself, which is what Seal requires.
type bareStore struct{ NodeStore }

// errSigner fails signing, the path a Builder and a durable recorder must propagate
// instead of emitting an unsigned record.
type errSigner struct{ err error }

func (s errSigner) SignCheckpoint(Checkpoint) (SignedCheckpoint, error) {
	return SignedCheckpoint{}, s.err
}
func (s errSigner) SignSnapshotClaim(SnapshotClaim) ([]byte, error) { return nil, s.err }
func (s errSigner) KeyID() string                                   { return "err-signer" }

// errCheckpointStore fails one or both halves of the checkpoint port.
type errCheckpointStore struct {
	*fakeCheckpointStore
	loadErr error
	saveErr error
}

func (s *errCheckpointStore) SaveCheckpoint(ctx context.Context, stream string, size uint64, cose []byte) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.fakeCheckpointStore.SaveCheckpoint(ctx, stream, size, cose)
}

func (s *errCheckpointStore) LatestCheckpoint(ctx context.Context, stream string) (uint64, []byte, bool, error) {
	if s.loadErr != nil {
		return 0, nil, false, s.loadErr
	}
	return s.fakeCheckpointStore.LatestCheckpoint(ctx, stream)
}

// scriptedLog is a spine.Log whose Append and Read are driven by a test: it can fail,
// or hand back an event the canonical encoder must refuse (a zero Time). Nothing here
// touches a real store.
type scriptedLog struct {
	appendErr error
	readErr   error
	appended  spine.Event
	events    []spine.Event
}

func (l *scriptedLog) Append(_ context.Context, in spine.AppendInput) (spine.Event, error) {
	if l.appendErr != nil {
		return spine.Event{}, l.appendErr
	}
	e := l.appended
	if e.Stream == "" {
		e.Stream = in.Stream
	}
	return e, nil
}

func (l *scriptedLog) Read(_ context.Context, q spine.Query) ([]spine.Event, error) {
	if l.readErr != nil {
		return nil, l.readErr
	}
	out := make([]spine.Event, 0, len(l.events))
	for _, e := range l.events {
		if e.Stream == q.Stream && e.Seq > q.AfterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *scriptedLog) SaveSnapshot(context.Context, spine.Snapshot) error { return nil }

func (l *scriptedLog) LatestSnapshot(context.Context, string, int64) (spine.Snapshot, bool, error) {
	return spine.Snapshot{}, false, nil
}

var _ spine.Log = (*scriptedLog)(nil)

// signRaw produces a COSE_Sign1 over an arbitrary payload under a given content type.
// It is how the tests forge the artifacts a verifier must reject: a checkpoint
// signature replayed as a snapshot, a signed payload that is not a checkpoint at all,
// or a signature carrying no key id.
func signRaw(t *testing.T, priv ed25519.PrivateKey, keyID, contentType string, payload []byte) []byte {
	t.Helper()
	protected := cose.ProtectedHeader{
		cose.HeaderLabelAlgorithm:   checkpointAlg,
		cose.HeaderLabelContentType: contentType,
	}
	if keyID != "" {
		protected[cose.HeaderLabelKeyID] = []byte(keyID)
	}
	msg, err := cose.Sign1(nil, ed25519CoseSigner{key: priv}, cose.Headers{Protected: protected}, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// TestTreeSurfacesStoreFailures is the storage-failure gate: a Tree never reports a
// successful append or a proof when the node store underneath it failed. A failed leaf
// write, a failed internal-node write, and a read failure or a missing node during
// proof assembly must all surface as errors, because a proof assembled from a partially
// written store would attest a root the log does not have.
func TestTreeSurfacesStoreFailures(t *testing.T) {
	cb, err := CanonicalBytes(sampleEvent())
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("store is down")

	t.Run("leaf write fails", func(t *testing.T) {
		st := newErrStore()
		st.putErr, st.putLevel = boom, 0
		tr := NewTreeWithStore(st)
		if err := tr.Append(cb); !errors.Is(err, boom) {
			t.Fatalf("append error = %v, want the store failure", err)
		}
		if tr.Size() != 0 {
			t.Fatalf("size = %d after a failed append, want 0", tr.Size())
		}
	})

	t.Run("internal node write fails", func(t *testing.T) {
		st := newErrStore()
		st.putErr, st.putLevel = boom, 1
		tr := NewTreeWithStore(st)
		// The first append completes no internal node; the second completes level 1.
		if err := tr.Append(cb); err != nil {
			t.Fatal(err)
		}
		second := sampleEvent()
		second.Seq = 8
		cb2, err := CanonicalBytes(second)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(cb2); !errors.Is(err, boom) {
			t.Fatalf("append error = %v, want the store failure", err)
		}
	})

	t.Run("proof assembly read fails", func(t *testing.T) {
		st := newErrStore()
		tr := NewTreeWithStore(st)
		for i := range 4 {
			e := sampleEvent()
			e.Seq = int64(i + 1)
			b, err := CanonicalBytes(e)
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Append(b); err != nil {
				t.Fatal(err)
			}
		}
		st.getErr = boom
		if _, err := tr.InclusionProof(0); !errors.Is(err, boom) {
			t.Fatalf("inclusion proof error = %v, want the store failure", err)
		}
		st.getErr, st.missAll = nil, true
		_, err := tr.InclusionProof(0)
		if !hasCode(err, CodeMissingNode) {
			t.Fatalf("inclusion proof over a store missing nodes: err = %v, want %s", err, CodeMissingNode)
		}
	})
}

// TestTreeRejectsOutOfRangeProofs asserts a Tree refuses to produce a proof it cannot
// honestly make: an inclusion proof for a leaf past its size, and a consistency proof
// to a size larger than the tree. Emitting a proof there would be a proof about a log
// that does not exist.
func TestTreeRejectsOutOfRangeProofs(t *testing.T) {
	tr, _ := buildTree(t, 5)

	if _, err := tr.InclusionProof(5); !hasCode(err, CodeInclusionInvalid) {
		t.Fatalf("inclusion proof at index == size: err = %v, want %s", err, CodeInclusionInvalid)
	}
	if _, err := tr.InclusionProof(99); !hasCode(err, CodeInclusionInvalid) {
		t.Fatalf("inclusion proof past the end: err = %v, want %s", err, CodeInclusionInvalid)
	}
	if _, err := tr.ConsistencyProof(6); !hasCode(err, CodeConsistencyInvalid) {
		t.Fatalf("consistency proof to a larger size: err = %v, want %s", err, CodeConsistencyInvalid)
	}
}

// TestLoadTreeRejectsIncompleteStore is the restart gate: reopening a log at a signed
// size from a store that cannot produce the frontier must fail rather than resume on a
// silently truncated tree, which would let later appends build on a root nobody signed.
func TestLoadTreeRejectsIncompleteStore(t *testing.T) {
	boom := errors.New("tile read failed")

	st := newErrStore()
	st.missAll = true
	if _, err := LoadTree(st, 4); !hasCode(err, CodeMissingNode) {
		t.Fatalf("LoadTree over an empty store: err = %v, want %s", err, CodeMissingNode)
	}

	st2 := newErrStore()
	st2.getErr = boom
	if _, err := LoadTree(st2, 4); !errors.Is(err, boom) {
		t.Fatalf("LoadTree error = %v, want the store failure", err)
	}

	// Size 0 needs no frontier node, so an empty store still reopens an empty tree.
	tr, err := LoadTree(newErrStore(), 0)
	if err != nil {
		t.Fatalf("LoadTree at size 0: %v", err)
	}
	if tr.Size() != 0 {
		t.Fatalf("size = %d, want 0", tr.Size())
	}
}

// TestCloneStoreRefusesUnsnapshottableStore asserts a tree over a store that cannot
// snapshot itself refuses to seal, rather than handing a SealedRun a node store that
// later appends can mutate underneath it.
func TestCloneStoreRefusesUnsnapshottableStore(t *testing.T) {
	tr := NewTreeWithStore(bareStore{newMemNodeStore()})
	if _, err := tr.cloneStore(); !hasCode(err, CodeEncode) {
		t.Fatalf("cloneStore over a bare store: err = %v, want %s", err, CodeEncode)
	}
}

// TestTiledNodeStoreReportsAbsentNodes asserts the tiled layout distinguishes an
// unwritten slot from a zero hash: a node in a tile that was never created, and a node
// past the filled prefix of an existing tile, both read as absent rather than as a
// valid all-zero hash.
func TestTiledNodeStoreReportsAbsentNodes(t *testing.T) {
	st := newTiledNodeStore()

	if _, ok, err := st.Node(0, 0); err != nil || ok {
		t.Fatalf("Node on an empty store = (ok=%v, err=%v), want absent", ok, err)
	}
	if err := st.PutNode(0, 0, bytes.Repeat([]byte{0xab}, hashSize)); err != nil {
		t.Fatal(err)
	}
	// Index 1 sits in the same tile but past its filled prefix.
	if _, ok, err := st.Node(0, 1); err != nil || ok {
		t.Fatalf("Node past the filled prefix = (ok=%v, err=%v), want absent", ok, err)
	}
	h, ok, err := st.Node(0, 0)
	if err != nil || !ok {
		t.Fatalf("Node(0,0) = (ok=%v, err=%v), want present", ok, err)
	}
	if !bytes.Equal(h, bytes.Repeat([]byte{0xab}, hashSize)) {
		t.Fatal("stored hash does not round trip through the tile")
	}
}

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

// TestValidUTF8ValueWalksNestedPayloads asserts the canonical encoder refuses an event
// whose payload hides an invalid UTF-8 string anywhere in it: in a nested map's key or
// value, in a slice element, or in a map keyed by any. A CBOR text string must be valid
// UTF-8, so encoding one would produce bytes that do not round trip.
func TestValidUTF8ValueWalksNestedPayloads(t *testing.T) {
	const bad = "\xff\xfe"

	tests := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"clean nested", map[string]any{
			"m": map[string]any{"k": "v"},
			"l": []any{"a", int64(1), []byte{0xff}},
		}, true},
		{"bytes are not text", map[string]any{"blob": []byte{0xff, 0xfe}}, true},
		{"bad nested map value", map[string]any{"m": map[string]any{"k": bad}}, false},
		{"bad nested map key", map[string]any{"m": map[string]any{bad: "v"}}, false},
		{"bad slice element", map[string]any{"l": []any{"ok", bad}}, false},
		{"bad element deep in a slice of maps", map[string]any{
			"l": []any{map[string]any{"k": bad}},
		}, false},
		{"bad key in a map keyed by any", map[string]any{
			"m": map[any]any{bad: "v"},
		}, false},
		{"bad value in a map keyed by any", map[string]any{
			"m": map[any]any{"k": bad},
		}, false},
		{"non-string key in a map keyed by any is skipped", map[string]any{
			"m": map[any]any{int64(1): "v"},
		}, true},
		{"top-level bad string", map[string]any{"s": bad}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validUTF8Value(tt.payload); got != tt.want {
				t.Fatalf("validUTF8Value = %v, want %v", got, tt.want)
			}
			e := sampleEvent()
			e.Payload = tt.payload
			_, err := CanonicalBytes(e)
			if tt.want && err != nil {
				t.Fatalf("CanonicalBytes rejected a valid payload: %v", err)
			}
			if !tt.want && !hasCode(err, CodeInvalidUTF8) {
				t.Fatalf("CanonicalBytes err = %v, want %s", err, CodeInvalidUTF8)
			}
		})
	}
}

// TestDecodeCanonicalRejectsIndefiniteLength asserts the strict decoder refuses
// indefinite-length CBOR items and names the fault. An indefinite-length encoding is
// one of the two ambiguities that would let two different byte strings claim to be the
// same event, so it must never decode.
func TestDecodeCanonicalRejectsIndefiniteLength(t *testing.T) {
	// 0xbf starts an indefinite-length map, 0xff breaks it: an empty map, encoded the
	// one way the canonical form forbids.
	indefinite := []byte{0xbf, 0xff}
	_, err := DecodeCanonical(indefinite)
	if !hasCode(err, CodeIndefiniteLength) {
		t.Fatalf("DecodeCanonical of an indefinite-length map: err = %v, want %s", err, CodeIndefiniteLength)
	}
	if err := VerifyCanonical(indefinite); !hasCode(err, CodeIndefiniteLength) {
		t.Fatalf("VerifyCanonical of an indefinite-length map: err = %v, want %s", err, CodeIndefiniteLength)
	}
}

// TestIntFieldAcceptsEveryIntegerEncoding asserts a payload integer reads back the same
// whether the event came straight from canonical CBOR (uint64), back through a JSON
// store (float64), or from memory (int/int64), and that a value that cannot be an int64
// is refused rather than wrapped to a negative.
func TestIntFieldAcceptsEveryIntegerEncoding(t *testing.T) {
	const key = "call"
	tests := []struct {
		name  string
		value any
		want  int64
		ok    bool
	}{
		{"int64", int64(7), 7, true},
		{"int", 7, 7, true},
		{"uint64", uint64(7), 7, true},
		{"float64 from a JSON round trip", float64(7), 7, true},
		{"uint64 past int64", uint64(1) << 63, 0, false},
		{"string", "7", 0, false},
		{"bool", true, 0, false},
		{"nil value", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := intField(map[string]any{key: tt.value}, key)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("intField = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
	if _, ok := intField(map[string]any{}, key); ok {
		t.Fatal("intField over an absent key must report not-ok")
	}
}

// TestStringFieldIsTypeSafe asserts a payload string field reads as empty when it is
// absent or carries some other type, so a mistyped record never reads as a valid name.
func TestStringFieldIsTypeSafe(t *testing.T) {
	if got := stringField(map[string]any{"a": "x"}, "a"); got != "x" {
		t.Fatalf("stringField = %q, want x", got)
	}
	if got := stringField(map[string]any{"a": int64(1)}, "a"); got != "" {
		t.Fatalf("stringField over a non-string = %q, want empty", got)
	}
	if got := stringField(map[string]any{}, "a"); got != "" {
		t.Fatalf("stringField over an absent key = %q, want empty", got)
	}
}

// TestPayloadNumbersAcrossEncodings asserts a provenance declaration reads back the same
// counts and rates from every encoding it round trips through (canonical CBOR, a JSON
// store, memory), and that an out-of-range count is clamped rather than wrapped
// negative, which would read as fewer attested events than none.
func TestPayloadNumbersAcrossEncodings(t *testing.T) {
	intTests := []struct {
		name  string
		value any
		want  int
	}{
		{"int", 5, 5},
		{"int64", int64(5), 5},
		{"uint64", uint64(5), 5},
		{"float64", float64(5), 5},
		{"string", "5", 0},
		{"absent", nil, 0},
	}
	for _, tt := range intTests {
		t.Run("int/"+tt.name, func(t *testing.T) {
			if got := payloadInt(tt.value); got != tt.want {
				t.Fatalf("payloadInt = %d, want %d", got, tt.want)
			}
		})
	}
	if got := payloadInt(uint64(1) << 63); got <= 0 {
		t.Fatalf("payloadInt of an oversized count = %d, want it clamped to a positive maximum", got)
	}

	floatTests := []struct {
		name  string
		value any
		want  float64
	}{
		{"float64", 0.5, 0.5},
		{"int", 1, 1},
		{"string", "1", 0},
	}
	for _, tt := range floatTests {
		t.Run("float/"+tt.name, func(t *testing.T) {
			if got := payloadFloat(tt.value); got != tt.want {
				t.Fatalf("payloadFloat = %v, want %v", got, tt.want)
			}
		})
	}

	countTests := []struct {
		name  string
		value any
		want  map[string]int
	}{
		{"map[string]any from JSON", map[string]any{"a": float64(2)}, map[string]int{"a": 2}},
		{"map[any]any from CBOR", map[any]any{"a": uint64(2)}, map[string]int{"a": 2}},
		{"non-string keys are dropped", map[any]any{int64(1): uint64(2)}, map[string]int{}},
		{"empty map[string]any", map[string]any{}, nil},
		{"empty map[any]any", map[any]any{}, nil},
		{"wrong type", "a", nil},
	}
	for _, tt := range countTests {
		t.Run("counts/"+tt.name, func(t *testing.T) {
			got := payloadCounts(tt.value)
			if len(got) != len(tt.want) {
				t.Fatalf("payloadCounts = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("payloadCounts = %v, want %v", got, tt.want)
				}
			}
		})
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

// hasCode reports whether err carries the given fault code.
func hasCode(err error, code string) bool {
	return err != nil && govCode(err) == code
}
