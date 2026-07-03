package chain

import (
	"context"
	"math"
	"sync"

	"github.com/ionalpha/flynn/spine"
)

// FlushNodeStore is a NodeStore that can force its buffered nodes to durable storage. A
// durable log flushes before it signs a checkpoint, so the checkpoint's size is always
// fully backed by persisted proof nodes.
type FlushNodeStore interface {
	NodeStore
	Flush() error
}

// CheckpointStore persists and recovers a stream's signed tree heads. A durable log
// records its authenticated length as it grows and recovers it after a restart.
type CheckpointStore interface {
	SaveCheckpoint(ctx context.Context, stream string, size uint64, cose []byte) error
	LatestCheckpoint(ctx context.Context, stream string) (size uint64, cose []byte, ok bool, err error)
}

// DurableRecorder wraps a spine.Log and maintains a per-stream RFC 6962 Merkle log whose
// proof nodes are paged to durable tiles, signing a checkpoint every so often. Unlike a
// RecordingLog, which accumulates a whole run in memory and seals it once, a
// DurableRecorder is built for a long-lived stream: its resident state is the append
// frontier plus the tiles not yet flushed, and its history is recovered after a restart
// from the latest checkpoint plus the events after it. It is itself a spine.Log, so it
// drops in wherever a Log is expected.
//
// Recording never fails an Append: the wrapped append is the source of truth, so a
// recording or checkpoint error is routed to an error handler and the event is still
// returned, exactly as the RecordingLog keeps producing events when its integrity layer
// stumbles. A stream is caught up from the log after a transient failure, and an explicit
// Checkpoint (used to seal a stream's tip) does surface its error.
type DurableRecorder struct {
	spine.Log

	signer      RootSigner
	origin      OriginFunc
	ckpts       CheckpointStore
	nodes       func(stream string) FlushNodeStore
	every       int
	onErr       func(error)
	maxResident int

	mu      sync.Mutex
	streams map[string]*durableStream
}

// defaultMaxResident bounds how many streams' live Merkle logs a recorder keeps in
// memory at once. A server that touches many streams over its life evicts the least
// recently loaded rather than holding a tree per stream forever; an evicted stream
// rebuilds from its checkpoint plus the log on the next append, so eviction is only a
// reload cost, never a loss.
const defaultMaxResident = 128

type durableStream struct {
	tree    *Tree
	store   FlushNodeStore
	pending int // events recorded since the last checkpoint
}

var _ spine.Log = (*DurableRecorder)(nil)

// NewDurableRecorder wraps inner. nodes provides the durable node store for a stream,
// ckpts persists its checkpoints, signer signs them, and originFor maps a stream to its
// checkpoint origin (nil uses the stream id). A checkpoint is written every `every`
// recorded events; a non-positive `every` checkpoints only on an explicit Checkpoint
// call.
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

// WithMaxResident overrides how many streams stay resident before the recorder evicts
// the excess. A non-positive n disables eviction (every touched stream stays resident).
// It returns the recorder for chaining.
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

// OnError sets the handler called when best-effort recording or an automatic checkpoint
// fails during Append. It returns the recorder for chaining. With no handler set, such an
// error is dropped (the stream still catches up from the log later).
func (r *DurableRecorder) OnError(fn func(error)) *DurableRecorder {
	r.onErr = fn
	return r
}

// Append delegates to the wrapped log, then records the assigned event's leaf into its
// stream's durable Merkle log, checkpointing when the cadence is reached. Recording is
// best effort: a failure never fails the append, it is routed to the error handler.
func (r *DurableRecorder) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	e, err := r.Log.Append(ctx, in)
	if err != nil {
		return e, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rerr := r.record(ctx, e); rerr != nil && r.onErr != nil {
		r.onErr(rerr)
	}
	return e, nil
}

// record folds one event into its stream's durable Merkle log and checkpoints on cadence.
// The caller holds r.mu.
func (r *DurableRecorder) record(ctx context.Context, e spine.Event) error {
	s, err := r.streamFor(ctx, e.Stream, e.Seq)
	if err != nil {
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
		return r.checkpointLocked(ctx, e.Stream, s)
	}
	return nil
}

// Checkpoint forces a signed checkpoint for a stream at its current recorded length,
// flushing the tiles first. It is how a caller seals the tail of a stream (at the end of
// a run, or before shutdown) so the log's full length is durably attested. A stream not
// yet touched in this process is loaded and caught up to its tip first.
func (r *DurableRecorder) Checkpoint(ctx context.Context, stream string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.streamFor(ctx, stream, math.MaxInt64)
	if err != nil {
		return err
	}
	return r.checkpointLocked(ctx, stream, s)
}

// CheckpointAll seals the tip of every stream recorded in this process, so a graceful
// shutdown persists the full length of each live stream. It returns the first error and
// keeps going, so one stream's failure does not skip the rest.
func (r *DurableRecorder) CheckpointAll(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for stream, s := range r.streams {
		if err := r.checkpointLocked(ctx, stream, s); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// streamFor returns the recorder's live Merkle log for a stream, loading it on first
// touch. It resumes from the latest checkpoint (rebuilding the frontier from the tiles)
// and folds in the events after it that the tiles do not yet cover, up to but excluding
// beforeSeq. Passing the current event's Seq brings the tree exactly to the state before
// that event, so the caller then appends it once; passing math.MaxInt64 catches the tree
// up to the stream's tip.
func (r *DurableRecorder) streamFor(ctx context.Context, stream string, beforeSeq int64) (*durableStream, error) {
	if s, ok := r.streams[stream]; ok {
		return s, nil
	}
	store := r.nodes(stream)
	size, _, ok, err := r.ckpts.LatestCheckpoint(ctx, stream)
	if err != nil {
		return nil, err
	}
	var tree *Tree
	var afterSeq int64
	if ok && size > 0 && size <= math.MaxInt64 {
		tree, err = LoadTree(store, size)
		if err != nil {
			return nil, err
		}
		afterSeq = int64(size)
	} else {
		tree = NewTreeWithStore(store)
	}
	// Fold in events the tree does not yet cover (after the checkpoint, before the
	// caller's event). The read is Seq-ordered, so stop at the first event at or past
	// the exclusive bound.
	events, err := r.Read(ctx, spine.Query{Stream: stream, AfterSeq: afterSeq})
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.Seq >= beforeSeq {
			break
		}
		cb, cerr := CanonicalBytes(e)
		if cerr != nil {
			return nil, cerr
		}
		if aerr := tree.Append(cb); aerr != nil {
			return nil, aerr
		}
	}
	s := &durableStream{tree: tree, store: store}
	r.streams[stream] = s
	r.evictExcessLocked(stream)
	return s, nil
}

// evictExcessLocked drops resident streams beyond the cap, never the one just loaded. An
// evicted stream reloads from its checkpoint plus the log when next appended to, so this
// bounds memory without losing any history. The caller holds r.mu.
func (r *DurableRecorder) evictExcessLocked(keep string) {
	if r.maxResident <= 0 {
		return
	}
	for len(r.streams) > r.maxResident {
		evicted := false
		for k := range r.streams {
			if k == keep {
				continue
			}
			delete(r.streams, k)
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

// checkpointLocked flushes the stream's tiles, signs its current head, and persists the
// checkpoint. The caller holds r.mu.
func (r *DurableRecorder) checkpointLocked(ctx context.Context, stream string, s *durableStream) error {
	if s.tree.Size() == 0 {
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
