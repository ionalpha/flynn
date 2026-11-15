package chain

import (
	"context"
	"math"
	"sync"

	"github.com/ionalpha/flynn/spine"
)

// FlushNodeStore is a [NodeStore] that can persist its buffered nodes. A
// checkpoint is signed only after a flush, so its size is always backed by
// persisted proof nodes.
type FlushNodeStore interface {
	NodeStore
	Flush() error
}

// CheckpointStore persists and recovers a stream's signed tree heads.
type CheckpointStore interface {
	SaveCheckpoint(ctx context.Context, stream string, size uint64, cose []byte) error
	// LatestCheckpoint reports ok false with a nil error for a stream never
	// checkpointed, so a fresh stream is distinct from a read failure.
	LatestCheckpoint(ctx context.Context, stream string) (size uint64, cose []byte, ok bool, err error)
}

// DurableRecorder wraps a [spine.Log] and keeps a per-stream RFC 6962 Merkle log
// with proof nodes paged to durable tiles, signing a checkpoint periodically.
// Unlike a [RecordingLog], which holds a whole run in memory, it suits a
// long-lived stream: resident state is the append frontier plus unflushed tiles,
// and a restart recovers from the latest checkpoint plus later events.
//
// Recording never fails an Append: the wrapped append is the source of truth, so
// a recording or checkpoint error goes to the error handler and the event is
// still returned. The stream catches up from the log after a transient failure.
// An explicit Checkpoint does surface its error.
type DurableRecorder struct {
	spine.Log

	signer      RootSigner
	origin      OriginFunc
	ckpts       CheckpointStore
	nodes       func(stream string) FlushNodeStore
	every       int
	onErr       func(error)
	maxResident int

	// mu guards only the streams map and the eviction tick, so one stream's
	// checkpoint does not block another. Per-stream work runs under
	// durableStream.mu.
	mu      sync.Mutex
	streams map[string]*durableStream
	tick    uint64 // monotonic last-touch source for LRU eviction
}

// defaultMaxResident bounds live Merkle logs in memory. The least recently
// touched is evicted and rebuilds on its next append, so eviction costs a
// reload, never history.
const defaultMaxResident = 128

type durableStream struct {
	// mu serializes this stream's load, append and checkpoint work. Streams
	// record concurrently, and it is never held with DurableRecorder.mu. A
	// stream has a single writer, so events fold in Seq order.
	mu      sync.Mutex
	loaded  bool
	tree    *Tree
	store   FlushNodeStore
	pending int // events recorded since the last checkpoint

	touch uint64 // last-touch tick for LRU eviction; guarded by DurableRecorder.mu
}

var _ spine.Log = (*DurableRecorder)(nil)

// NewDurableRecorder wraps inner. originFor maps a stream to its checkpoint
// origin (nil uses the stream id). A checkpoint is written every `every` events;
// a non-positive `every` checkpoints only on an explicit Checkpoint.
func NewDurableRecorder(inner spine.Log, nodes func(stream string) FlushNodeStore, ckpts CheckpointStore, signer RootSigner, originFor OriginFunc, every int) *DurableRecorder {
	return &DurableRecorder{
		Log:         inner,
		signer:      signer,
		origin:      originFor,
		ckpts:       ckpts,
		nodes:       nodes,
		every:       every,
		maxResident: defaultMaxResident,
		streams:     map[string]*durableStream{},
	}
}

// WithMaxResident sets how many streams stay resident. A non-positive n
// disables eviction.
func (r *DurableRecorder) WithMaxResident(n int) *DurableRecorder {
	r.maxResident = n
	return r
}

func (r *DurableRecorder) originFor(stream string) string {
	if r.origin != nil {
		return r.origin(stream)
	}
	return stream
}

// OnError sets the handler for a recording or automatic checkpoint failure
// during Append. Without one the error is dropped.
func (r *DurableRecorder) OnError(fn func(error)) *DurableRecorder {
	r.onErr = fn
	return r
}

// Append delegates to the wrapped log, then records the leaf, checkpointing on
// cadence. A recording failure never fails the append.
func (r *DurableRecorder) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	e, err := r.Log.Append(ctx, in)
	if err != nil {
		return e, err
	}
	if rerr := r.record(ctx, e); rerr != nil && r.onErr != nil {
		r.onErr(rerr)
	}
	return e, nil
}

// record takes the global lock only to resolve the handle; the rest runs under
// the stream's own lock.
func (r *DurableRecorder) record(ctx context.Context, e spine.Event) error {
	s := r.handle(e.Stream)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.ensureLoaded(ctx, e.Stream, s, e.Seq); err != nil {
		return err
	}
	cb, err := CanonicalBytes(e)
	if err != nil {
		return err
	}
	if err := s.tree.Append(cb); err != nil {
		return err
	}
	s.pending++
	if r.every > 0 && s.pending >= r.every {
		return r.checkpointStream(ctx, e.Stream, s)
	}
	return nil
}

// handle marks the stream most recently used. A new handle has no tree until
// ensureLoaded runs, so this does no I/O under r.mu.
func (r *DurableRecorder) handle(stream string) *durableStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tick++
	s := r.streams[stream]
	if s == nil {
		s = &durableStream{}
		r.streams[stream] = s
	}
	s.touch = r.tick
	r.evictExcessLocked(stream)
	return s
}

// Checkpoint signs the stream's current length after flushing tiles, to seal a
// tail at the end of a run or before shutdown. A stream not yet touched in this
// process is first loaded and caught up to its tip.
func (r *DurableRecorder) Checkpoint(ctx context.Context, stream string) error {
	s := r.handle(stream)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.ensureLoaded(ctx, stream, s, math.MaxInt64); err != nil {
		return err
	}
	return r.checkpointStream(ctx, stream, s)
}

// CheckpointAll seals every stream recorded in this process, for graceful
// shutdown. It returns the first error and keeps going. It snapshots the
// handles under the global lock, then checkpoints each under its own, so no lock
// spans another stream's I/O.
func (r *DurableRecorder) CheckpointAll(ctx context.Context) error {
	r.mu.Lock()
	type entry struct {
		stream string
		s      *durableStream
	}
	entries := make([]entry, 0, len(r.streams))
	for stream, s := range r.streams {
		entries = append(entries, entry{stream, s})
	}
	r.mu.Unlock()

	var first error
	for _, e := range entries {
		if err := func() error {
			e.s.mu.Lock()
			defer e.s.mu.Unlock()
			if err := r.ensureLoaded(ctx, e.stream, e.s, math.MaxInt64); err != nil {
				return err
			}
			return r.checkpointStream(ctx, e.stream, e.s)
		}(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ensureLoaded resumes from the latest checkpoint and folds in events the tiles
// do not cover, up to but excluding beforeSeq. The current event's Seq brings the
// tree to just before it, so the caller appends it once; math.MaxInt64 reaches
// the tip. It runs off the global lock, so a cold reload does not block resident
// streams. The caller holds s.mu.
func (r *DurableRecorder) ensureLoaded(ctx context.Context, stream string, s *durableStream, beforeSeq int64) error {
	if s.loaded {
		return nil
	}
	store := r.nodes(stream)
	size, _, ok, err := r.ckpts.LatestCheckpoint(ctx, stream)
	if err != nil {
		return err
	}
	var tree *Tree
	var afterSeq int64
	if ok && size > 0 && size <= math.MaxInt64 {
		tree, err = LoadTree(store, size)
		if err != nil {
			return err
		}
		afterSeq = int64(size)
	} else {
		tree = NewTreeWithStore(store)
	}
	// The read is Seq-ordered, so stop at the first event at or past the bound.
	events, err := r.Read(ctx, spine.Query{Stream: stream, AfterSeq: afterSeq})
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Seq >= beforeSeq {
			break
		}
		cb, cerr := CanonicalBytes(e)
		if cerr != nil {
			return cerr
		}
		if aerr := tree.Append(cb); aerr != nil {
			return aerr
		}
	}
	s.tree = tree
	s.store = store
	s.loaded = true
	return nil
}

// evictExcessLocked drops the coldest streams beyond the cap, never keep. A
// stream has a single writer, so its next append, which reloads it, starts after
// the previous returned: two live trees never share tiles. The caller holds r.mu.
func (r *DurableRecorder) evictExcessLocked(keep string) {
	if r.maxResident <= 0 {
		return
	}
	for len(r.streams) > r.maxResident {
		var victim string
		var oldest uint64
		found := false
		for k, s := range r.streams {
			if k == keep {
				continue
			}
			if !found || s.touch < oldest {
				victim, oldest, found = k, s.touch, true
			}
		}
		if !found {
			return
		}
		delete(r.streams, victim)
	}
}

// checkpointStream runs without the global lock, so it does not stall other
// streams. The caller holds s.mu. An unloaded stream has nothing to seal.
func (r *DurableRecorder) checkpointStream(ctx context.Context, stream string, s *durableStream) error {
	if !s.loaded || s.tree.Size() == 0 {
		return nil
	}
	if err := s.store.Flush(); err != nil {
		return err
	}
	root, err := s.tree.Root()
	if err != nil {
		return err
	}
	sc, err := r.signer.SignCheckpoint(Checkpoint{Origin: r.originFor(stream), Size: s.tree.Size(), RootHash: root})
	if err != nil {
		return err
	}
	if err := r.ckpts.SaveCheckpoint(ctx, stream, s.tree.Size(), sc.COSE); err != nil {
		return err
	}
	s.pending = 0
	return nil
}
