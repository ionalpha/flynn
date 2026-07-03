package spine

import "context"

// Snapshot is a materialized projection of a stream up to and including Seq: a
// checkpoint a Fold resumes from instead of replaying the stream from the start,
// so reading state stays fast as a stream grows without bound. Payload is the
// domain's serialized state, opaque to the spine, so every projection (the
// resource store, a session) snapshots through one mechanism.
//
// A snapshot never replaces events: the log stays the immutable source of truth,
// and a snapshot is a derived cache that can always be rebuilt by folding from an
// earlier point. So a missing snapshot is only slower, never wrong, and the spine
// Log keeps the snapshots alongside the events it checkpoints.
type Snapshot struct {
	Stream  string
	Seq     int64
	Payload []byte
}

// SnapshotCodec transforms snapshots between their projection form and their
// stored form, so a store can persist verified snapshots without the spine
// depending on any cryptography. Seal wraps a projection payload before it is
// saved (binding it to the log prefix it derives from); Open verifies and unwraps
// a stored payload before it is restored. A store with no codec stores payloads
// as-is. The chain package provides the signing implementation; an Open failure
// means the snapshot must not be trusted and the reader folds from the start
// instead - a rejected snapshot is only slower, never wrong.
type SnapshotCodec interface {
	Seal(ctx context.Context, log Log, s Snapshot) (Snapshot, error)
	Open(ctx context.Context, s Snapshot) (Snapshot, error)
}
