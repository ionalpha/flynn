package spine

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
