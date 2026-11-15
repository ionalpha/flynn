package spine

import "context"

// Snapshot is a projection of a stream up to and including Seq, so a Fold can
// resume there instead of replaying from the start. Payload is opaque to the
// spine. A snapshot is a cache rebuildable from the events, so a missing one
// is slower and never wrong.
type Snapshot struct {
	Stream  string
	Seq     int64
	Payload []byte
}

// SnapshotCodec lets a store persist verified snapshots without the spine
// depending on cryptography. Seal binds a payload to the log prefix it derives
// from before saving; Open verifies and unwraps it before restoring. On an Open
// error the reader distrusts the snapshot and folds from the start. The chain
// package provides the signing codec; with none, payloads are stored as-is.
type SnapshotCodec interface {
	Seal(ctx context.Context, log Log, s Snapshot) (Snapshot, error)
	Open(ctx context.Context, s Snapshot) (Snapshot, error)
}
