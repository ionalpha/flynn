package guard

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Store wraps a state.MemoryStore with the write-time ingest gate: content from an
// untrusted source that carries a screening hit (a hidden-instruction payload or
// overt injection phrasing) is refused before it is ever persisted, so it can never
// be recalled later. It is a decorator, so a host opts in by wrapping its own store
// and the underlying persistence, recall ranking, and provenance are unchanged.
//
// The gate fires only for untrusted-origin content. The agent's own run and the
// operator's own instructions are allowed through even if they trip the phrase
// screen, because a legitimate note may quote an injection string (a note about an
// attack, a captured lesson); refusing those would tax honest work with no security
// gain, since trusted-origin content is not the poisoning vector. This mirrors the
// package thesis: trust is the wall, the phrase screen is a bar-raiser.
type Store struct {
	inner state.MemoryStore
	audit func(context.Context, Refusal)
}

// Refusal is the record of a refused write, passed to the audit callback so a host
// can append it to the spine (poison attempts leave a trail).
type Refusal struct {
	// Sources is the refused item's full provenance, not just the source that made
	// it untrusted: an audit of a poisoning attempt wants every input the write
	// claimed, including the trusted ones it was mixed with.
	Sources  []string
	Trust    sandbox.Trust
	Findings []Finding
}

// Option configures a Store.
type Option func(*Store)

// WithAudit registers a callback invoked for every refused write, so the host can
// record the attempt on the append-only spine. The callback runs before Write
// returns its error. Nil callbacks are ignored.
func WithAudit(fn func(context.Context, Refusal)) Option {
	return func(s *Store) {
		if fn != nil {
			s.audit = fn
		}
	}
}

// Wrap returns a Store guarding inner. With no options it refuses poisoned
// untrusted writes silently (the error is the only signal); add WithAudit to record
// attempts.
func Wrap(inner state.MemoryStore, opts ...Option) *Store {
	s := &Store{inner: inner}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ state.MemoryStore = (*Store)(nil)

// Write screens the item and refuses an untrusted-origin write that carries a
// screening hit, returning a Forbidden fault; otherwise it records the write's taint
// and delegates to the inner store. The screen runs on the item's content; trust
// comes from its Sources via TrustOfAll, which takes the weakest of them, so mixing
// one trusted input into a distilled item does not buy it past the gate.
//
// Taint is recorded here rather than left to the caller because this is the one
// place every guarded write passes through while the writing context is still in
// hand (see TaintItem). It is not a refusal: a tainted item is stored and recalled
// normally, and only the wake digest treats it differently.
func (s *Store) Write(ctx context.Context, m state.MemoryItem) (state.MemoryItem, error) {
	trust := TrustOfAll(m.Sources)
	if trust == sandbox.TrustUntrusted {
		if findings := Screen(m.Content); len(findings) > 0 {
			if s.audit != nil {
				s.audit(ctx, Refusal{Sources: m.Sources, Trust: trust, Findings: findings})
			}
			return state.MemoryItem{}, fault.New(fault.Forbidden, "memory_poison_refused",
				"refused to persist untrusted-origin memory carrying a hidden-instruction payload: "+findings[0].Detail)
		}
	}
	return s.inner.Write(ctx, TaintItem(ctx, m))
}

// Recall delegates unchanged. Retrieval-side trust is available to callers via
// TrustOfAll on each item's Sources, so a governance gate can treat an untrusted-origin
// memory as data rather than as the agent's vetted intent without this store having
// to alter the recall contract.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	return s.inner.Recall(ctx, q)
}

// Delete delegates unchanged.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

// RecordPush delegates unchanged. The gate is a write-time ingest screen on
// content, and usage carries none: an item that got past the screen is an item
// this store has no further opinion about, and refusing to count reads of it would
// leave the selection policy blind on exactly the corpus the guard admitted.
func (s *Store) RecordPush(ctx context.Context, memoryIDs []string) error {
	return s.inner.RecordPush(ctx, memoryIDs)
}

// RecordUse delegates unchanged, origin included.
func (s *Store) RecordUse(ctx context.Context, memoryID string, origin state.UsageOrigin) error {
	return s.inner.RecordUse(ctx, memoryID, origin)
}

// Usage delegates unchanged.
func (s *Store) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	return s.inner.Usage(ctx, memoryIDs)
}
