package chain

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// OriginFunc maps a stream id to the origin a sealed record is scoped to. A nil func
// uses the stream id itself.
type OriginFunc func(stream string) string

// RecordingLog wraps a spine.Log and records every appended event into a per-stream
// verifiable chain, so a stream can be sealed into a signed run record without
// changing how events are produced. It is itself a spine.Log, so it drops in wherever
// a Log is expected.
//
// Recording never fails an Append: a recording error is retained and surfaced at
// Seal. A run is therefore never broken by the integrity layer, but a stream whose
// recording failed (a non-canonical event) cannot be sealed, so a sealed record
// always covers the stream's complete, canonical event sequence.
type RecordingLog struct {
	spine.Log

	origin OriginFunc

	mu       sync.Mutex
	builders map[string]*streamRecorder
}

type streamRecorder struct {
	mu      sync.Mutex // guards builder+err; encode+hash runs here, not under RecordingLog.mu
	builder *Builder
	err     error
}

// NewRecordingLog wraps inner. originFor maps a stream to its record origin; if nil,
// the stream id is used.
func NewRecordingLog(inner spine.Log, originFor OriginFunc) *RecordingLog {
	return &RecordingLog{Log: inner, origin: originFor, builders: map[string]*streamRecorder{}}
}

func (r *RecordingLog) originFor(stream string) string {
	if r.origin != nil {
		return r.origin(stream)
	}
	return stream
}

// Append delegates to the wrapped log, then records the assigned event into its
// stream's chain. The append result is returned unchanged; a recording failure is
// kept for Seal rather than propagated, so producing events never depends on the
// integrity layer succeeding.
func (r *RecordingLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	e, err := r.Log.Append(ctx, in)
	if err != nil {
		return e, err
	}
	// The global lock only fetches (or creates) the stream's recorder; the encode+hash
	// runs under that recorder's own lock, so appends to different streams do not
	// serialize behind one another on fan-out.
	sr := r.recorderFor(e.Stream)
	sr.mu.Lock()
	if sr.err == nil {
		sr.err = sr.builder.Add(e)
	}
	sr.mu.Unlock()
	return e, nil
}

// recorderFor returns the recorder for a stream, creating it on first use. It holds
// the global lock only for the map access.
func (r *RecordingLog) recorderFor(stream string) *streamRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	sr := r.builders[stream]
	if sr == nil {
		sr = &streamRecorder{builder: NewBuilder(r.originFor(stream))}
		r.builders[stream] = sr
	}
	return sr
}

// Seal signs and returns the sealed record for a stream as recorded so far. It fails
// if the stream was never recorded or if recording any of its events failed, so a
// sealed record always covers the stream's full, canonical event sequence. The
// returned record is an immutable snapshot: further appends to the stream do not
// change it.
func (r *RecordingLog) Seal(stream string, signer RootSigner) (*SealedRun, error) {
	sr := r.lookup(stream)
	if sr == nil {
		return nil, fault.New(fault.Terminal, CodeEmptyRecord, "chain: stream was not recorded")
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.err != nil {
		return nil, sr.err
	}
	return sr.builder.Seal(signer)
}

// SealAndReset seals the stream's record and resets its recorder, so a long-lived
// stream can rotate into bounded segments: the returned record owns everything
// recorded so far, and later appends accumulate into a fresh run under the same
// origin. It refuses under the same conditions as Seal.
func (r *RecordingLog) SealAndReset(stream string, signer RootSigner) (*SealedRun, error) {
	sr := r.lookup(stream)
	if sr == nil {
		return nil, fault.New(fault.Terminal, CodeEmptyRecord, "chain: stream was not recorded")
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sr.err != nil {
		return nil, sr.err
	}
	return sr.builder.SealAndReset(signer)
}

// lookup returns the recorder for a stream, or nil if the stream was never recorded.
// It holds the global lock only for the map access.
func (r *RecordingLog) lookup(stream string) *streamRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.builders[stream]
}

var _ spine.Log = (*RecordingLog)(nil)
