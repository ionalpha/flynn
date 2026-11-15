package guard

import (
	"context"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Store wraps a state.MemoryStore and refuses, before persisting, untrusted content
// with a screening hit, so it can never be recalled. A host opts in by wrapping its
// store; persistence, ranking and provenance are unchanged.
//
// Only untrusted-origin content is gated. The agent's and operator's notes may quote
// an injection string, such as a note about an attack, and they are not the
// poisoning vector.
type Store struct {
	inner state.MemoryStore
	audit func(context.Context, Refusal)
}

// Refusal records a refused write for the audit callback.
type Refusal struct {
	// Sources is the full provenance, trusted sources included, because an audit
	// wants every input the write claimed.
	Sources  []string
	Trust    sandbox.Trust
	Findings []Finding
}

// Option configures a Store.
type Option func(*Store)

// WithAudit sets a callback for each refused write, run before Write returns its
// error, so the host can record the attempt. Nil is ignored.
func WithAudit(fn func(context.Context, Refusal)) Option {
	return func(s *Store) {
		if fn != nil {
			s.audit = fn
		}
	}
}

// Wrap returns a Store guarding inner. Without [WithAudit] the returned error is
// the only record of a refusal.
func Wrap(inner state.MemoryStore, opts ...Option) *Store {
	s := &Store{inner: inner}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ state.MemoryStore = (*Store)(nil)

// Write refuses untrusted content with a screening hit with a Forbidden fault.
// Trust is [TrustOfAll], so one trusted source does not carry an item past the gate.
//
// Otherwise it records taint ([TaintItem]) and writes. Taint is set here because
// every guarded write passes through with its context. A tainted item is stored and
// recalled normally; only the digest excludes it.
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

// Recall delegates unchanged. Callers derive trust with [TrustOfAll] on each item.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	return s.inner.Recall(ctx, q)
}

// Delete delegates unchanged.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

// RecordPush delegates unchanged, because usage carries no content to screen.
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

// Promote delegates unchanged. [PushEligibility] denies tainted and untrusted items
// on read whatever the decision says.
func (s *Store) Promote(ctx context.Context, d state.PromotionDecision) (state.MemoryPromotion, error) {
	return s.inner.Promote(ctx, d)
}

// Promotions delegates unchanged.
func (s *Store) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	return s.inner.Promotions(ctx, memoryIDs)
}
