// Package ridealong attaches what memory knows about a thing to the read of that
// thing. A host serving a task, an entity or a document calls Surface with the
// anchor refs it already has in hand, and the memories anchored to them come back
// with the response. That is cue-based associative recall: the wake digest carries
// the standing context a session needs every time, and this carries the fact that
// applies right now, to the thing being looked at.
//
// It answers the case a search box cannot. Pulling a memory by query requires
// already suspecting the fact exists; a reader who has never seen it has no query
// to type. An anchor is a cue the host already holds, so the fact arrives without
// anybody guessing at it.
//
// The second reason this package exists is measurement. Every surfacing and every
// recall taken through here records a use against the item, classified by whether
// the reader had already been shown it (see PrimeScope). Without that split the
// decay system cannot tell an item that earns its place from one that is only ever
// seen because the digest keeps putting it there, and last-used-at degrades into a
// record of what was pushed.
//
// Host neutrality. An anchor is an opaque {Kind, ID} pair. Nothing here resolves
// one, and no ref kind is named or special-cased: the vocabulary is the host's, and
// an anchor pointing at something that no longer exists simply matches nothing.
package ridealong

import (
	"context"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// defaultLimit caps a surfacing that names no limit of its own. A ride-along rides
// on somebody else's response, so it has to be bounded by default: an uncapped one
// would attach every memory anchored to a busy ref to every read of it. Five is the
// working guess at "enough to be useful, small enough to read", and a caller that
// knows better sets RecallQuery.Limit.
const defaultLimit = 5

// ErrUsageNotRecorded reports that memories were surfaced but the use could not be
// counted. It is returned alongside the items, never instead of them: the reader
// asked for memory and the memory is real, so failing the read would trade the
// product for the instrumentation. A caller that only wants the items can ignore an
// error matching this; a caller that reads the decay metrics wants to know its
// numbers just went under-counted.
var ErrUsageNotRecorded = errors.New("ridealong: usage not recorded")

// Admission is how much of what an anchor matches may ride along on a read.
//
// A surfacing is content arriving with no question behind it, which is the property
// that makes the wake digest worth attacking (see package memory/guard). It is a
// narrower channel than the digest: it fires only when the reader opens the thing
// the memory is anchored to, it is capped, and every delivery is counted. It is
// still a channel, and an attacker who can write an anchored memory chooses the ref
// it rides on, so it gets a gate of its own rather than the recall path's silence.
type Admission int

const (
	// AdmitUndenied drops what guard.PushEligibility denies outright: a tainted item,
	// or one whose provenance is untrusted. Those two categories are
	// attacker-influenced by construction, so they never arrive at a reader who did
	// not ask; they stay recallable, with their provenance, by a reader who does.
	//
	// It is the zero value and the default. The agent's own untainted notes do ride
	// along unpromoted, which is where this is deliberately more permissive than the
	// digest: a cue-bound, capped delivery to a reader already looking at the anchored
	// thing is a smaller grant than a line in every wake of every session forever.
	AdmitUndenied Admission = iota
	// AdmitPushable admits only what the wake digest itself could carry: the
	// operator's own untainted memory, plus what a named reviewer has promoted. It
	// costs a promotion lookup for the items whose answer turns on one. Choose it to
	// hold every unasked-for delivery to a single standard.
	AdmitPushable
	// AdmitAll admits everything the anchors matched, including tainted and
	// untrusted-origin items. It is for a host that renders a surfaced memory with
	// its provenance visible and treats it as data to be judged rather than as
	// something the agent knows, and for tooling that surfaces exactly what a
	// curator would need to review.
	AdmitAll
)

// Surfacer reads memory on behalf of a host's own read surfaces and keeps the usage
// signal honest while doing it. It holds no state of its own beyond the store, the
// cap and the admission policy; what a run has been shown lives on the context
// (NewPrimeScope), because that is the span the answer is true for.
type Surfacer struct {
	store     state.MemoryStore
	limit     int
	admission Admission
}

// Option configures a Surfacer.
type Option func(*Surfacer)

// WithLimit sets the cap applied to a surfacing that names no limit of its own.
// A non-positive n restores the default; there is no way to ask for an uncapped
// surfacing, because the caller that genuinely wants every anchored item is doing a
// recall and can say so with RecallQuery.Limit.
func WithLimit(n int) Option {
	return func(s *Surfacer) {
		if n <= 0 {
			n = defaultLimit
		}
		s.limit = n
	}
}

// WithAdmission sets how much of what an anchor matches may ride along. An
// unrecognised value is refused at construction rather than silently treated as the
// default, because the two ends of this setting differ by exactly the content an
// attacker gets to place.
func WithAdmission(a Admission) Option {
	return func(s *Surfacer) {
		switch a {
		case AdmitUndenied, AdmitPushable, AdmitAll:
			s.admission = a
		default:
			panic(fmt.Sprintf("ridealong: unknown admission %d", int(a)))
		}
	}
}

// New returns a Surfacer reading through store.
func New(store state.MemoryStore, opts ...Option) *Surfacer {
	s := &Surfacer{store: store, limit: defaultLimit}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Surface returns the memories anchored to the query's refs, most recent first,
// admitted by the Surfacer's policy, and counts a use of each one it returns.
//
// The query must carry at least one anchor. An anchorless surfacing is refused with
// state.ErrInvalid rather than widened into an unfiltered recall: a ride-along with
// no cue behind it would attach an arbitrary slice of the corpus to a read nobody
// aimed at memory, and count every row of it as used.
//
// Everything else on the query is honoured as-is, so a host can narrow a surfacing
// by scope, kind or time window the same way it narrows any other recall. A query
// with no Limit gets the Surfacer's cap.
//
// The cap bounds the read and the admission policy then drops from what came back,
// so a surfacing can return fewer items than the cap while further anchored items
// exist. Reading past the cap to refill it would let the presence of inadmissible
// items decide how much of the corpus a single read touches, which is a lever an
// attacker holds and the reader does not.
func (s *Surfacer) Surface(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	anchors, err := state.NormalizeAnchors(q.Anchors)
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("%w: surfacing needs at least one anchor", state.ErrInvalid)
	}
	q.Anchors = anchors
	if q.Limit <= 0 {
		q.Limit = s.limit
	}
	return s.read(ctx, q, s.admit)
}

// Recall runs an explicit recall and counts a use of everything it returns. It is
// the pull half of the same measurement: a session that goes looking for a fact and
// finds it has used that fact, and the decay policy needs that recorded as surely as
// it needs the push.
//
// The admission policy does not apply here, and no recall path anywhere applies
// one. A reader that asks a question gets the whole answer, tainted and
// untrusted-origin items included, carrying the provenance that says what they are.
// Filtering a search would hide from a curator exactly the records that most need
// looking at, and the gate exists for content nobody asked for.
//
// One use is counted per returned item, which is one write per item. A caller that
// wants a read with no measurement behind it (a curator listing the corpus, a
// backup, a report) reads the store directly; going through here would file the
// tooling's reads as though a session had acted on them.
func (s *Surfacer) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	anchors, err := state.NormalizeAnchors(q.Anchors)
	if err != nil {
		return nil, err
	}
	q.Anchors = anchors
	return s.read(ctx, q, nil)
}

// Push counts a push of these items and records them on ctx's prime scope, so a use
// of one later in the run is attributed as primed rather than as the reader finding
// it unaided. It is the call the digest builder makes when it hands a digest to a
// session.
//
// The two halves are here together on purpose. A store that counted the push while
// the run forgot it had happened would go on crediting the digest's own items as
// organically recalled, which is the exact measurement this whole subsystem exists
// to protect. The scope is marked only after the store accepted the push, because a
// push that failed did not reach the reader.
func (s *Surfacer) Push(ctx context.Context, memoryIDs []string) error {
	if err := s.store.RecordPush(ctx, memoryIDs); err != nil {
		return err
	}
	MarkPushed(ctx, memoryIDs...)
	return nil
}

// admit applies the Surfacer's admission policy to a recall's results.
func (s *Surfacer) admit(ctx context.Context, in []state.MemoryItem) ([]state.MemoryItem, error) {
	switch s.admission {
	case AdmitAll:
		return in, nil
	case AdmitPushable:
		return guard.Pushable(ctx, s.store, in)
	default:
		// A new slice, never a filter in place: the result belongs to the store, and
		// compacting it would write through whatever backing array it handed over.
		out := make([]state.MemoryItem, 0, len(in))
		for _, it := range in {
			if guard.PushEligibility(it) != guard.PushDenied {
				out = append(out, it)
			}
		}
		return out, nil
	}
}

// read recalls, applies the gate when there is one, and counts a use of each result
// at its origin. Only what the reader ends up with is counted: an item the gate
// dropped never reached anybody.
//
// Recording is best effort per item and never rewrites the result: an item that was
// tombstoned between the recall and the count is skipped (it was really returned,
// and there is nothing left to count it against), and any other failure is collected
// and reported under ErrUsageNotRecorded with the items still returned.
func (s *Surfacer) read(ctx context.Context, q state.RecallQuery, gate func(context.Context, []state.MemoryItem) ([]state.MemoryItem, error)) ([]state.MemoryItem, error) {
	items, err := s.store.Recall(ctx, q)
	if err != nil {
		return nil, err
	}
	if gate != nil {
		if items, err = gate(ctx, items); err != nil {
			return nil, err
		}
	}
	var failures []error
	for _, it := range items {
		err := s.store.RecordUse(ctx, it.ID, OriginFor(ctx, it.ID))
		switch {
		case err == nil, errors.Is(err, state.ErrNotFound):
		default:
			failures = append(failures, fmt.Errorf("memory %s: %w", it.ID, err))
		}
	}
	if len(failures) > 0 {
		return items, fmt.Errorf("%w: %w", ErrUsageNotRecorded, errors.Join(failures...))
	}
	return items, nil
}
