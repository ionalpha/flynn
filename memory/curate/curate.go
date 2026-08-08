// Package curate decides what a new memory does to the memory already stored.
//
// Replace-on-subject is right for state and wrong for experience. A preference,
// a decision or a stated fact has one current answer, and a store that keeps
// every revision of it hands a reader five contradictory answers and no way to
// tell which one is in force. A failure, an attempt, an observation is the
// opposite: the fifth failure matters precisely because there were four before
// it, and replacing them turns an error trail into amnesia at the moment it was
// about to become a lesson.
//
// So the semantics are split by kind, over one subject key:
//
//   - Replace kinds (fact, preference, decision by default) supersede the
//     standing item of the same kind on the same subject and scope, and retire it.
//     The replacement records what it replaced (MemoryItem.Supersedes) and the
//     loser is tombstoned, so the correction is in the record and the retirement
//     leaves its own trail.
//   - Append kinds (everything else) join the series under the subject and retire
//     nothing. The series is what a consolidation pass later distils into a
//     lesson.
//
// Two write-path protections ride along with the split, both of which exist
// because the failure they prevent is silent:
//
//   - An agent's conclusion never silently replaces a more trusted fact. A
//     replace-class write whose provenance is weaker than what it would retire is
//     demoted to an append, and the contradiction is recorded as its own episode
//     for a curator to resolve. Nothing is refused and nothing is lost; what the
//     agent may not do is quietly overwrite the operator.
//   - A subject that looks like a fork of one already in use is flagged. "db-choice"
//     and "database-choice" are one topic spelled two ways, and once both exist the
//     replace chain has forked in a way no later read can detect.
//
// What this package does not do is decide anything the store could not tell it.
// The kind vocabulary is configurable (WithClass), similarity is pluggable
// (WithSimilarity), and both protections report through a callback rather than
// choosing a log format for a host.
package curate

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Class is the write semantics a memory kind carries: what a new item of that
// kind does to the items already stored under its subject.
type Class int

const (
	// ClassAppend joins the series under the subject and retires nothing. It is the
	// zero value and the default for an unrecognised kind, because appending is the
	// answer that cannot lose anything: a store that guessed replace on a kind it did
	// not know would delete the record to find out it had guessed wrong.
	ClassAppend Class = iota
	// ClassReplace supersedes the standing item of the same kind on the same subject
	// and scope, and retires it.
	ClassReplace
)

// String renders the class for a message a person reads.
func (c Class) String() string {
	if c == ClassReplace {
		return "replace"
	}
	return "append"
}

// Kinds carrying replace semantics by default: the three that assert a current
// answer rather than record something that happened. A host with its own
// vocabulary overrides this per kind with WithClass.
const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindDecision   = "decision"
)

// KindConflict is the kind written for a contradiction that was recorded rather
// than resolved: an agent concluded something that contradicts a fact it is not
// trusted to overwrite. It is an append kind, and it carries the subject of the
// conflict so the curator's read of that subject finds it.
const KindConflict = "conflict"

// defaultClasses is the built-in kind vocabulary. Anything absent appends.
var defaultClasses = map[string]Class{
	KindFact:       ClassReplace,
	KindPreference: ClassReplace,
	KindDecision:   ClassReplace,
}

// ClassOf reports the semantics of a kind under the built-in vocabulary. It is
// exported because the split is a fact about the kinds and not only about this
// store: a consolidation pass deciding which series are worth distilling, or a
// review queue explaining why one write retired a fact and another did not, is
// asking this question without a store in hand. A store with its own vocabulary
// answers for itself (see Store.ClassOf).
func ClassOf(kind string) Class { return defaultClasses[kind] }

// defaultSimilarityThreshold is how alike two subjects have to look before a write
// reports them as a possible fork. It is tuned against the case the split cares
// about - "db-choice" against "database-choice", one topic spelled two ways - and
// against the pairs that must not fire, like "db-choice" against "queue-choice".
//
// The number only means anything against the similarity function it is paired
// with, so a host supplying its own (WithSimilarity, typically an embedding
// distance) sets its own threshold with it.
const defaultSimilarityThreshold = 0.6

// Notice is something the write path noticed and did not refuse: a contradiction
// it recorded instead of acting on, or a subject that looks like a fork. It is
// reported through WithNotify so a host can log, alert or queue it for review
// without this package choosing a format, level or sink.
type Notice struct {
	// Kind names what was noticed.
	Kind NoticeKind
	// Subject is the normalized subject of the write that triggered it.
	Subject string
	// Scope is the scope the write landed in.
	Scope state.Scope
	// Incoming is the item as written, after any demotion. Its ID is the stored id,
	// so a reviewer can go and read it.
	Incoming state.MemoryItem
	// Conflicting is the stored item the incoming write contradicted, for
	// NoticeCrossTrust. It is the zero item for a fork notice.
	Conflicting state.MemoryItem
	// Similar is the existing subject the incoming one looks like a fork of, for
	// NoticeForkedSubject. It is empty for a cross-trust notice.
	Similar string
	// Detail is the one-line description a person reads.
	Detail string
}

// NoticeKind names the class of a Notice.
type NoticeKind string

const (
	// NoticeCrossTrust is a replace-class write that was demoted to an append
	// because it would have retired a more trusted item.
	NoticeCrossTrust NoticeKind = "cross-trust-replacement"
	// NoticeForkedSubject is a subject new to its scope that looks like a spelling
	// of one already in use.
	NoticeForkedSubject NoticeKind = "forked-subject"
)

// Store wraps a state.MemoryStore with the write policy. Recall and every other
// method delegate unchanged: this decides what a write means, and has no opinion
// about reads.
//
// It composes with the other memory decorators rather than replacing them. A host
// that wants both the poison gate and these semantics wraps guard.Store in this
// one, so a refused write never reaches the policy and a policy write is screened
// like any other.
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

// WithClass sets the semantics of one kind, overriding the default vocabulary. It
// is how a host names its own kinds ("runbook" replaces, "incident" appends)
// without this package enumerating somebody else's domain. An empty kind is
// ignored.
func WithClass(kind string, c Class) Option {
	return func(s *Store) {
		if kind != "" {
			s.classes[kind] = c
		}
	}
}

// WithNotify registers a callback for everything the write path noticed and did
// not refuse (see Notice). The callback runs before Write returns, and its errors
// are its own: a notice is an observation, and failing the write over one would
// lose the memory to the reporting of it. Nil callbacks are ignored.
func WithNotify(fn func(context.Context, Notice)) Option {
	return func(s *Store) {
		if fn != nil {
			s.notify = fn
		}
	}
}

// WithSimilarity replaces the subject similarity measure, which decides whether a
// new subject is reported as a fork of an existing one. It must return a score in
// [0,1] with 1 identical, and it must be symmetric, because the pair it is asked
// about arrives in whichever order the store listed them.
//
// The built-in measure is lexical (see SubjectSimilarity). A host with embeddings
// in hand passes a distance over them, which catches the pair the lexical measure
// cannot: two spellings that share no substring at all. Pass the matching
// threshold with WithSimilarityThreshold, since a score only means something
// against the function that produced it. A nil function is ignored.
func WithSimilarity(fn func(a, b string) float64) Option {
	return func(s *Store) {
		if fn != nil {
			s.similar = fn
		}
	}
}

// WithSimilarityThreshold sets the score at or above which two subjects are
// reported as a possible fork. A threshold above 1 switches fork detection off,
// which is the honest way for a host with no useful measure to opt out rather
// than supplying a function that always returns zero.
func WithSimilarityThreshold(t float64) Option {
	return func(s *Store) { s.threshold = t }
}

// Wrap returns a Store applying the write policy over inner. With no options it
// uses the built-in kind vocabulary and lexical fork detection, and reports
// nothing anywhere: add WithNotify to see what it noticed.
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

// Write applies the policy and persists the item.
//
// An item with no subject is written unchanged. There is no series to reason
// about and nothing to key a replacement on, so the policy has no question to
// answer, and inventing a subject from the content would be the store guessing at
// what a fact is about.
//
// A subjected write reads the live items on its subject and scope first. Scope is
// matched exactly, never widened: a workspace's own decision does not retire the
// project-wide one it inherits, because the outer fact is still true for every
// other workspace under it. Overriding an inherited fact is what writing at the
// inner scope already does, through recall's most-specific-first order.
//
// The write is persisted before anything is retired. A store cannot offer one
// transaction across two calls, so the order is chosen for what a crash between
// them leaves behind: this way the new fact is stored and the old one is still
// live, which a later pass can finish, where the other order would delete the
// standing answer and then fail to store its replacement.
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
		// A fork can only start at the moment a subject first appears in a scope, so
		// the scan for one runs then and not on every write to an established subject.
		s.checkFork(ctx, m)
	}

	if s.classes[m.Kind] != ClassReplace {
		return s.inner.Write(ctx, m)
	}

	// Only the same kind on the same subject is a candidate: a decision about the
	// database does not retire the preference stated about it, and a reader asking
	// for the standing preference would find it gone.
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
			// The replacement is stored and says what it replaced, so the record is
			// intact and the caller can see both. Reporting it as a write failure would
			// invite a retry that writes the fact a second time.
			return written, fmt.Errorf("curate: retire superseded item %s: %w", it.ID, err)
		}
	}
	return written, nil
}

// seriesAt reads the live items under a subject in exactly this scope. The scope
// filter is applied here rather than left to the query because a zero Scope means
// "every scope" to Recall, which is the right default for a search and the wrong
// one for a policy: a global write would otherwise collect the whole fleet's items
// on that subject and retire them.
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

// moreTrustedThan reports the first standing item the incoming write is not
// trusted enough to retire, if there is one. Trust is derived from provenance
// (guard.TrustOfAll), which takes the weakest source of each side, so a
// conclusion mixed from an operator's instruction and a fetched page is judged on
// the page.
//
// Equal trust replaces. A second write from the operator revising their own
// preference is the ordinary case, and a rule that demanded strictly greater
// trust would make the standing answer permanent once written.
func moreTrustedThan(m state.MemoryItem, standing []state.MemoryItem) (state.MemoryItem, bool) {
	incoming := guard.TrustOfAll(m.Sources)
	for _, it := range standing {
		// sandbox.Trust ascends from trusted to untrusted, so a smaller value on the
		// stored item is a fact the incoming write is not entitled to overwrite.
		if guard.TrustOfAll(it.Sources) < incoming {
			return it, true
		}
	}
	return state.MemoryItem{}, false
}

// recordConflict stores a demoted write and the episode that says what it
// contradicted. The demoted item keeps its own kind and subject and supersedes
// nothing, so it reads as one more claim on the subject rather than as the answer.
//
// The episode is written after the item and carries its id, so the curator reading
// the conflict can reach both sides. A failure to write the episode is returned
// with the stored item rather than instead of it: the agent's conclusion is real
// memory and belongs in the store even when the note about it did not land.
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
		// The episode is about both sides, so it credits both and supersedes neither.
		// Its own provenance is the incoming write's: a note about an untrusted
		// conclusion is not more trustworthy than the conclusion.
		Sources: m.Sources,
		Anchors: m.Anchors,
		Tainted: m.Tainted,
	}
	if _, err := s.inner.Write(ctx, episode); err != nil {
		return written, fmt.Errorf("curate: record the contradiction on subject %s: %w", m.Subject, err)
	}
	return written, nil
}

// checkFork reports a subject that is new to its scope and looks like a spelling
// of one already in use there. It only ever reports: a store that refused the
// write would be asserting that its similarity measure is right, and a lexical
// measure is a heuristic that fires on "prod-deploy" against "prod-deployment"
// and misses two words for one thing that share no letters.
//
// A threshold above 1 disables the scan entirely, including the read it needs, so
// opting out costs nothing per write.
func (s *Store) checkFork(ctx context.Context, m state.MemoryItem) {
	if s.notify == nil || s.threshold > 1 {
		return
	}
	items, err := s.inner.Recall(ctx, state.RecallQuery{Scope: m.Scope})
	if err != nil {
		// A fork notice is an observation about the store, and failing a write because
		// the observation could not be made would trade the memory for the warning.
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

// trustName renders a trust level for a message a person reads.
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

// truncate bounds a quoted content excerpt so one long memory cannot turn the
// conflict episode into a copy of it. It cuts on a rune boundary, so a multi-byte
// character is never split into mojibake in a record nobody will re-derive.
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

// Recall delegates unchanged. The policy is about what a write means; a reader
// still sees every live item, including two that contradict each other, and
// decides for itself.
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	return s.inner.Recall(ctx, q)
}

// Delete delegates unchanged. Retiring an item by hand is exactly what a host does
// to resolve a conflict this package recorded, so it is not the policy's to
// second-guess.
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
