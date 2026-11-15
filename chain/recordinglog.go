package chain

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/spine"
)

// OriginFunc maps a stream id to the origin a sealed record is scoped to. Nil
// uses the stream id.
type OriginFunc func(stream string) string

// RecordingLog wraps a [spine.Log] and records every appended event into a
// per-stream chain that can be sealed into a signed run record.
//
// Recording never fails an Append: a recording error is retained and surfaced at
// Seal. A stream whose recording failed (a non-canonical event) cannot be sealed,
// so a sealed record always covers the complete, canonical event sequence.
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

// NewRecordingLog wraps inner. A nil originFor uses the stream id.
func NewRecordingLog(inner spine.Log, originFor OriginFunc) *RecordingLog {
	return &RecordingLog{Log: inner, origin: originFor, builders: map[string]*streamRecorder{}}
}

func (r *RecordingLog) originFor(stream string) string {
	if r.origin != nil {
		return r.origin(stream)
	}
	return stream
}

// Append returns the wrapped log's result unchanged; a recording failure is kept
// for Seal.
func (r *RecordingLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	e, err := r.Log.Append(ctx, in)
	if err != nil {
		return e, err
	}
	// The global lock only fetches the recorder; encode and hash run under its own
	// lock, so streams do not serialize behind one another.
	sr := r.recorderFor(e.Stream)
	sr.mu.Lock()
	if sr.err == nil {
		sr.err = sr.builder.Add(e)
	}
	sr.mu.Unlock()
	return e, nil
}

// recorderFor creates the recorder on first use, holding the global lock only for
// the map access.
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

// Seal signs the stream as recorded so far. It fails if the stream was never
// recorded or any event failed to record. Later appends do not change the result.
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

// SealAndReset seals the stream and starts a fresh run under the same origin, so
// a long-lived stream rotates into bounded segments. It refuses as Seal does.
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

// lookup returns nil if the stream was never recorded.
func (r *RecordingLog) lookup(stream string) *streamRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.builders[stream]
}

var _ spine.Log = (*RecordingLog)(nil)
