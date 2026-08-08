package chain

// The shared fault-injection harness for this package's failure tests. Every fake here
// exists to make one dependency fail on demand: a node store that refuses a write or a
// read, a signer that cannot sign, a checkpoint store that loses either half of its
// port, and a log whose Append and Read are scripted. The failure tests themselves live
// in the faults_*_test.go files alongside this one.

import (
	"context"
	"crypto/ed25519"
	"testing"

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

// hasCode reports whether err carries the given fault code.
func hasCode(err error, code string) bool {
	return err != nil && govCode(err) == code
}
