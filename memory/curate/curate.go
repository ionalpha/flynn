// Package curate decides what a new memory does to the memory already stored.
// A fact, preference or decision has one current answer, so keeping every revision
// gives a reader contradictions. A failure or observation is the opposite: the
// fifth failure is evidence because of the four before it.
//
// The semantics split by kind over one subject key:
//
//   - Replace kinds (fact, preference, decision by default) supersede the standing
//     item of the same kind, subject and scope. The new item lists it in
//     MemoryItem.Supersedes and the old one is tombstoned.
//   - Append kinds (all others) join the series under the subject, which
//     consolidation later distils into a lesson.
//
// Two protections guard failures nobody would see:
//
//   - A replace write with weaker provenance than the item it would retire is
//     demoted to an append, and the contradiction is recorded for a curator. An
//     agent cannot overwrite the operator.
//   - A new subject that resembles one in use ("db-choice", "database-choice") is
//     reported, because a forked replace chain is undetectable later.
//
// The vocabulary ([WithClass]) and similarity ([WithSimilarity]) are configurable,
// and both protections report through [WithNotify].
package curate

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Class is what a new item of a kind does to the items under its subject.
type Class int

const (
	// ClassAppend retires nothing. It is the zero value and the default for an
	// unknown kind, because appending cannot lose a record.
	ClassAppend Class = iota
	// ClassReplace retires the standing item of the same kind, subject and scope.
	ClassReplace
)

// String returns "replace" or "append".
func (c Class) String() string {
	if c == ClassReplace {
		return "replace"
	}
	return "append"
}

// Kinds that replace by default, because each asserts a current answer. Override
// per kind with [WithClass].
const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindDecision   = "decision"
)

// KindConflict records a write that contradicted a fact it was not trusted to
// overwrite. It appends under the conflict's subject, so a read of that subject
// finds it.
const KindConflict = "conflict"

// defaultClasses is the built-in kind vocabulary. Anything absent appends.
var defaultClasses = map[string]Class{
	KindFact:       ClassReplace,
	KindPreference: ClassReplace,
	KindDecision:   ClassReplace,
}

// ClassOf reports a kind's class under the built-in vocabulary, for callers with no
// store, such as consolidation. [Store.ClassOf] uses the store's vocabulary.
func ClassOf(kind string) Class { return defaultClasses[kind] }

// defaultSimilarityThreshold is tuned so "db-choice" and "database-choice" fire
// and "db-choice" and "queue-choice" do not. It suits only [SubjectSimilarity]; a
// host replacing the measure sets its own threshold.
const defaultSimilarityThreshold = 0.6

// Notice is a contradiction or a likely fork the write path reported without
// refusing the write, delivered through [WithNotify].
type Notice struct {
	// Kind names what was noticed.
	Kind NoticeKind
	// Subject is the normalized subject of the write that triggered it.
	Subject string
	// Scope is the scope the write landed in.
	Scope state.Scope
	// Incoming is the item as written, after any demotion.
	Incoming state.MemoryItem
	// Conflicting is the contradicted item, for [NoticeCrossTrust] only.
	Conflicting state.MemoryItem
	// Similar is the subject it resembles, for [NoticeForkedSubject] only.
	Similar string
	// Detail is a one-line description.
	Detail string
}

// NoticeKind names what a [Notice] reports.
type NoticeKind string

const (
	// NoticeCrossTrust is a replace write demoted because it would retire a more
	// trusted item.
	NoticeCrossTrust NoticeKind = "cross-trust-replacement"
	// NoticeForkedSubject is a new subject resembling one in use in its scope.
	NoticeForkedSubject NoticeKind = "forked-subject"
)

// Store applies the write policy over a state.MemoryStore; every other method
// delegates. Wrap a guard.Store in it so a refused write never reaches the policy
// and every write the policy makes is screened.
type Store struct {
	inner     state.MemoryStore
	classes   map[string]Class
	notify    func(context.Context, Notice)
	similar   func(a, b string) float64
	threshold float64
}

var _ state.MemoryStore = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithClass sets one kind's class, such as "runbook" replacing. An empty kind is
// ignored.
func WithClass(kind string, c Class) Option {
	return func(s *Store) {
		if kind != "" {
			s.classes[kind] = c
		}
	}
}

// WithNotify registers a callback for each [Notice], run before Write returns. It
// cannot fail the write, so reporting never loses a memory. Nil is ignored.
func WithNotify(fn func(context.Context, Notice)) Option {
	return func(s *Store) {
		if fn != nil {
			s.notify = fn
		}
	}
}

// WithSimilarity replaces [SubjectSimilarity] for fork detection. fn must return a
// score in [0,1], 1 identical, and be symmetric, because pairs arrive in list order.
// An embedding distance catches synonyms the lexical measure misses. Set a matching
// [WithSimilarityThreshold]. A nil function is ignored.
func WithSimilarity(fn func(a, b string) float64) Option {
	return func(s *Store) {
		if fn != nil {
			s.similar = fn
		}
	}
}

// WithSimilarityThreshold sets the score at or above which a fork is reported. Above
// 1 turns fork detection off.
func WithSimilarityThreshold(t float64) Option {
	return func(s *Store) { s.threshold = t }
}

// Wrap returns a Store over inner with the built-in vocabulary and lexical fork
// detection. Without [WithNotify] nothing is reported.
func Wrap(inner state.MemoryStore, opts ...Option) *Store {
	s := &Store{
		inner:     inner,
		classes:   make(map[string]Class, len(defaultClasses)),
		similar:   SubjectSimilarity,
		threshold: defaultSimilarityThreshold,
	}
	for k, c := range defaultClasses {
		s.classes[k] = c
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ClassOf reports the semantics of a kind under this store's vocabulary.
func (s *Store) ClassOf(kind string) Class { return s.classes[kind] }

// Write applies the policy and stores m. An item with no subject is written
// unchanged. Scope matches exactly: a workspace decision does not retire the
// project-wide one, which still holds for other workspaces, and recall already
// prefers the inner scope.
//
// The new item is stored before the old is retired, because there is no transaction
// across the calls. A crash between them leaves both live, which a later pass can
// finish; the other order could lose the answer.
func (s *Store) Write(ctx context.Context, m state.MemoryItem) (state.MemoryItem, error) {
	subject, err := state.NormalizeSubject(m.Subject)
	if err != nil {
		return state.MemoryItem{}, err
	}
	m.Subject = subject
	if subject == "" {
		return s.inner.Write(ctx, m)
	}

	series, err := s.seriesAt(ctx, subject, m.Scope)
	if err != nil {
		return state.MemoryItem{}, err
	}
	if len(series) == 0 {
		// A fork starts when a subject first appears in a scope, so only then is it
		// scanned for.
		s.checkFork(ctx, m)
	}

	if s.classes[m.Kind] != ClassReplace {
		return s.inner.Write(ctx, m)
	}

	// Only the same kind is replaced: a decision does not retire a preference on
	// the same subject.
	standing := make([]state.MemoryItem, 0, len(series))
	for _, it := range series {
		if it.Kind == m.Kind {
			standing = append(standing, it)
		}
	}
	if blocker, ok := moreTrustedThan(m, standing); ok {
		return s.recordConflict(ctx, m, blocker)
	}

	for _, it := range standing {
		m.Supersedes = append(m.Supersedes, it.ID)
	}
	written, err := s.inner.Write(ctx, m)
	if err != nil {
		return state.MemoryItem{}, err
	}
	for _, it := range standing {
		if err := s.inner.Delete(ctx, it.ID); err != nil {
			// The replacement is stored and returned, so a retry does not write the
			// fact twice.
			return written, fmt.Errorf("curate: retire superseded item %s: %w", it.ID, err)
		}
	}
	return written, nil
}

// seriesAt reads the live items under subject in exactly scope. It filters here
// because a zero Scope means every scope to Recall, and a global write would retire
// the whole fleet's items.
func (s *Store) seriesAt(ctx context.Context, subject string, scope state.Scope) ([]state.MemoryItem, error) {
	items, err := s.inner.Recall(ctx, state.RecallQuery{Subjects: []string{subject}, Scope: scope})
	if err != nil {
		return nil, err
	}
	out := items[:0]
	for _, it := range items {
		if it.Scope == scope {
			out = append(out, it)
		}
	}
	return out, nil
}

// moreTrustedThan returns the first standing item m may not retire. Trust is each
// side's weakest source (guard.TrustOfAll), so a conclusion mixing an operator's
// words and a fetched page is judged as the page. Equal trust replaces, or an
// operator could never revise their own preference.
func moreTrustedThan(m state.MemoryItem, standing []state.MemoryItem) (state.MemoryItem, bool) {
	incoming := guard.TrustOfAll(m.Sources)
	for _, it := range standing {
		// sandbox.Trust ascends from trusted to untrusted.
		if guard.TrustOfAll(it.Sources) < incoming {
			return it, true
		}
	}
	return state.MemoryItem{}, false
}

// recordConflict stores m, superseding nothing, then a [KindConflict] episode naming
// both sides. If the episode fails, the stored item is still returned with the
// error, because the conclusion is memory either way.
func (s *Store) recordConflict(ctx context.Context, m state.MemoryItem, blocker state.MemoryItem) (state.MemoryItem, error) {
	written, err := s.inner.Write(ctx, m)
	if err != nil {
		return state.MemoryItem{}, err
	}
	detail := fmt.Sprintf("a %s write concluded %q, which contradicts the more trusted %s %s",
		trustName(guard.TrustOfAll(m.Sources)), truncate(m.Content, 120), trustName(guard.TrustOfAll(blocker.Sources)), blocker.ID)
	s.report(ctx, Notice{
		Kind: NoticeCrossTrust, Subject: m.Subject, Scope: m.Scope,
		Incoming: written, Conflicting: blocker, Detail: detail,
	})
	episode := state.MemoryItem{
		Kind:    KindConflict,
		Subject: m.Subject,
		Scope:   m.Scope,
		Content: detail,
		// The incoming write's provenance, because a note about an untrusted
		// conclusion is no more trusted than it.
		Sources: m.Sources,
		Anchors: m.Anchors,
		Tainted: m.Tainted,
	}
	if _, err := s.inner.Write(ctx, episode); err != nil {
		return written, fmt.Errorf("curate: record the contradiction on subject %s: %w", m.Subject, err)
	}
	return written, nil
}

// checkFork reports, and never refuses, because the measure is a heuristic. A
// threshold above 1 skips the read too, so opting out costs nothing.
func (s *Store) checkFork(ctx context.Context, m state.MemoryItem) {
	if s.notify == nil || s.threshold > 1 {
		return
	}
	items, err := s.inner.Recall(ctx, state.RecallQuery{Scope: m.Scope})
	if err != nil {
		// A failed observation must not fail the write.
		return
	}
	best, score := "", 0.0
	for _, it := range items {
		if it.Scope != m.Scope || it.Subject == "" || it.Subject == m.Subject {
			continue
		}
		if sc := s.similar(m.Subject, it.Subject); sc > score {
			best, score = it.Subject, sc
		}
	}
	if best == "" || score < s.threshold {
		return
	}
	s.report(ctx, Notice{
		Kind: NoticeForkedSubject, Subject: m.Subject, Scope: m.Scope, Incoming: m, Similar: best,
		Detail: fmt.Sprintf("new subject %q closely resembles %q already in use in this scope", m.Subject, best),
	})
}

func (s *Store) report(ctx context.Context, n Notice) {
	if s.notify != nil {
		s.notify(ctx, n)
	}
}

func trustName(t sandbox.Trust) string {
	switch t {
	case sandbox.TrustTrusted:
		return "trusted"
	case sandbox.TrustUntrusted:
		return "untrusted"
	default:
		return "semi-trusted"
	}
}

// truncate cuts s to limit runes, on a rune boundary so no character is split.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return strings.TrimSpace(string(r[:limit])) + "..."
}

// Recall delegates unchanged, so a reader sees contradicting items too.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	return s.inner.Recall(ctx, q)
}

// Delete delegates unchanged; it is how a host resolves a recorded conflict.
func (s *Store) Delete(ctx context.Context, id string) error { return s.inner.Delete(ctx, id) }

// RecordPush delegates unchanged.
func (s *Store) RecordPush(ctx context.Context, memoryIDs []string) error {
	return s.inner.RecordPush(ctx, memoryIDs)
}

// RecordUse delegates unchanged.
func (s *Store) RecordUse(ctx context.Context, memoryID string, origin state.UsageOrigin) error {
	return s.inner.RecordUse(ctx, memoryID, origin)
}

// Usage delegates unchanged.
func (s *Store) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	return s.inner.Usage(ctx, memoryIDs)
}

// Promote delegates unchanged.
func (s *Store) Promote(ctx context.Context, d state.PromotionDecision) (state.MemoryPromotion, error) {
	return s.inner.Promote(ctx, d)
}

// Promotions delegates unchanged.
func (s *Store) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	return s.inner.Promotions(ctx, memoryIDs)
}
