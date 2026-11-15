// Package ridealong attaches the memories anchored to a thing to the read of that
// thing. A host serving a task or document calls [Surfacer.Surface] with the anchor
// refs it holds, so a fact arrives without the reader needing a query for it.
//
// Every surfacing and recall through here records a use, classified by whether the
// reader was already shown the item (see [NewPrimeScope]). Without that split, decay
// cannot tell a useful item from one the digest keeps pushing.
//
// An anchor is an opaque {Kind, ID} pair that nothing here resolves; one pointing at
// something gone matches nothing.
package ridealong

import (
	"context"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// defaultLimit caps a surfacing with no Limit, because an uncapped one attaches
// every memory on a busy ref to every read of it.
const defaultLimit = 5

// ErrUsageNotRecorded reports a use that was not counted. It is returned with the
// items, never instead, so measurement never fails a read.
var ErrUsageNotRecorded = errors.New("ridealong: usage not recorded")

// Admission is how much of what an anchor matches may ride along. A surfacing is
// unasked content like the digest (see memory/guard), though narrower: cue-bound,
// capped and counted. An attacker writing an anchored memory picks its ref, so it
// has a gate.
type Admission int

const (
	// AdmitUndenied is the default: it drops what guard.PushEligibility denies, tainted
	// or untrusted items, which stay recallable. The agent's own untainted notes
	// ride along unpromoted, more permissive than the digest because a capped,
	// cue-bound delivery is a smaller grant than a line in every wake.
	AdmitUndenied Admission = iota
	// AdmitPushable admits only what the digest could carry, at the cost of a
	// promotion lookup.
	AdmitPushable
	// AdmitAll admits everything, for a host that shows provenance and treats the
	// items as data, and for curator tooling.
	AdmitAll
)

// Surfacer reads memory for a host and records each use. What a run was shown is
// kept on the context ([NewPrimeScope]), the span it is true for.
type Surfacer struct {
	store     state.MemoryStore
	limit     int
	admission Admission
}

// Option configures a Surfacer.
type Option func(*Surfacer)

// WithLimit sets the default cap. A non-positive n restores the default; there is
// no uncapped surfacing, because that is a recall.
func WithLimit(n int) Option {
	return func(s *Surfacer) {
		if n <= 0 {
			n = defaultLimit
		}
		s.limit = n
	}
}

// WithAdmission sets the [Admission]. An unknown value panics, because guessing
// would decide what attacker content gets through.
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

// Surface returns admitted memories anchored to q's refs, newest first, and counts
// a use of each. A query with no anchor fails with state.ErrInvalid, because an
// unfiltered recall would attach arbitrary memory and count it all as used.
//
// Admission filters after the capped read, so fewer than the cap may return. Reading
// on to refill would let inadmissible items decide how much a read touches.
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

// Recall runs a recall and counts a use of each item, one write each. No admission
// applies: an asking reader gets tainted items with their provenance, so a curator
// sees what most needs review. Tooling that should not count reads the store
// directly.
func (s *Surfacer) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	anchors, err := state.NormalizeAnchors(q.Anchors)
	if err != nil {
		return nil, err
	}
	q.Anchors = anchors
	return s.read(ctx, q, nil)
}

// Push counts a push and marks the items on ctx's prime scope, so a later use counts
// as primed. Both happen here so they cannot drift apart. The scope is marked only
// after the store accepts the push, because a failed push reached nobody.
func (s *Surfacer) Push(ctx context.Context, memoryIDs []string) error {
	if err := s.store.RecordPush(ctx, memoryIDs); err != nil {
		return err
	}
	MarkPushed(ctx, memoryIDs...)
	return nil
}

func (s *Surfacer) admit(ctx context.Context, in []state.MemoryItem) ([]state.MemoryItem, error) {
	switch s.admission {
	case AdmitAll:
		return in, nil
	case AdmitPushable:
		return guard.Pushable(ctx, s.store, in)
	default:
		// A new slice, because filtering in place would write through the store's
		// backing array.
		out := make([]state.MemoryItem, 0, len(in))
		for _, it := range in {
			if guard.PushEligibility(it) != guard.PushDenied {
				out = append(out, it)
			}
		}
		return out, nil
	}
}

// read counts a use only for items past the gate. An item tombstoned since the
// recall is skipped; other failures are joined under [ErrUsageNotRecorded] with the
// items still returned.
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
