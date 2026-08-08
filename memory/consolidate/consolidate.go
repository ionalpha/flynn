// Package consolidate turns a subject's accumulated episodes into one lesson.
//
// It is the other half of the write-semantics split (see memory/curate). Episode
// kinds append, so a subject that keeps going wrong accumulates a series: five
// failures of the same deploy, each recorded when it happened. The series is
// worth keeping while it is being written, because the fifth failure only means
// something against the four before it. It is not worth keeping forever: nobody
// reads five near-identical episodes, and a digest that offered them would spend
// its whole budget saying one thing five times.
//
// Consolidation is where that becomes learning. The pass distils the series into
// one lesson, records the lesson as superseding the episodes it was drawn from,
// and tombstones them. What is left on the subject is one item that says what was
// learned, which is the line a wake digest can afford to carry.
//
// Distilling is the host's. A lesson is a language judgment about what a series
// means, and this package has no model and no business pretending to summarize.
// The caller supplies a Distiller; everything around it - which series are ready,
// what the lesson inherits, what gets retired, and what a re-run does - is here,
// because those are the parts that have to be right every time and identically.
//
// # Running it again
//
// The pass is idempotent and resumable, which matters because it is offline work
// that can be killed halfway. The lesson is written before the episodes are
// retired, so a run interrupted between the two leaves a lesson that names
// episodes still live. A later run recognises that state and finishes the
// retirement rather than distilling a second lesson from the same series, so
// running the pass twice produces what running it once would have.
package consolidate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/ionalpha/flynn/state"
)

// KindLesson is the kind a distilled lesson is written under, and KindEpisode is
// the kind the pass reads by default. Both are the memory vocabulary the write
// policy already uses; a host with its own names sets them (WithLessonKind,
// WithEpisodeKinds).
const (
	KindLesson  = "lesson"
	KindEpisode = "episode"
)

// defaultMinEpisodes is how many episodes a subject needs before its series is
// worth distilling. Two is a coincidence and three is a pattern: consolidating a
// pair would spend a model call to compress a series a reader could have read.
const defaultMinEpisodes = 3

// Series is one subject's episodes, handed to a Distiller. Episodes are oldest
// first, because the series is a narrative and its order is the information: what
// changed between the first failure and the last is most of what a lesson says.
type Series struct {
	Subject  string
	Scope    state.Scope
	Episodes []state.MemoryItem
}

// Lesson is what a Distiller produces: the distilled content, and nothing else.
// Provenance, anchors, taint and expiry are derived from the series by the pass
// rather than restated here, so a distiller cannot accidentally launder a tainted
// episode into a clean lesson or drop the sources a purge follows.
type Lesson struct {
	// Content is the lesson. An empty Content declines the series: the pass writes
	// nothing, retires nothing, and reports the subject as declined. That is the
	// honest answer for a series a distiller cannot draw anything from, and it is
	// safe to give, because declining costs a re-read on the next run and deleting
	// the evidence would cost the series.
	Content string
}

// Distiller turns a series into a lesson. It is the one part of consolidation a
// host has to supply.
//
// It may be called for many subjects in one run, so an implementation backed by a
// model is responsible for its own rate limiting and budget. An error from it
// fails that subject and no other (see Report.Failures).
type Distiller interface {
	Distil(ctx context.Context, in Series) (Lesson, error)
}

// DistillerFunc adapts a function to Distiller.
type DistillerFunc func(ctx context.Context, in Series) (Lesson, error)

// Distil calls f.
func (f DistillerFunc) Distil(ctx context.Context, in Series) (Lesson, error) { return f(ctx, in) }

// ErrNoDistiller reports that the pass was built without one. It is returned at
// construction time rather than on the first run, so a host wires it up wrong
// once instead of discovering it on a nightly job.
var ErrNoDistiller = errors.New("consolidate: a distiller is required")

// Outcome is what the pass did with one subject.
type Outcome string

const (
	// OutcomeDistilled wrote a lesson and retired the episodes it was drawn from.
	OutcomeDistilled Outcome = "distilled"
	// OutcomeResumed found a lesson that already superseded the episodes in hand,
	// left by an interrupted run, and finished retiring them.
	OutcomeResumed Outcome = "resumed"
	// OutcomeTooFew left the series alone: not enough episodes yet.
	OutcomeTooFew Outcome = "too-few"
	// OutcomeDeclined left the series alone: the distiller returned no content.
	OutcomeDeclined Outcome = "declined"
)

// Result is what happened to one subject in one run.
type Result struct {
	Subject string
	Scope   state.Scope
	Outcome Outcome
	// Episodes is how many episodes were in hand when the subject was considered.
	Episodes int
	// Lesson is the item written, or the one an interrupted run had already
	// written. It is the zero item when nothing was distilled.
	Lesson state.MemoryItem
	// Retired is the ids tombstoned, in the order they were retired.
	Retired []string
}

// Failure is one subject the pass could not finish, and why. A sweep reports
// these rather than returning on the first one: an offline pass over a whole
// store must not lose two hundred healthy subjects to one that a model call
// timed out on.
type Failure struct {
	Subject string
	Scope   state.Scope
	Err     error
}

// Report is the outcome of one Run.
type Report struct {
	Results  []Result
	Failures []Failure
}

// Distilled counts the subjects a run actually consolidated, which is the number
// a host logs or alerts on. Resumed subjects are counted separately by Resumed:
// they are work finished, not learning done.
func (r Report) Distilled() int { return r.count(OutcomeDistilled) }

// Resumed counts the subjects whose interrupted retirement this run finished.
func (r Report) Resumed() int { return r.count(OutcomeResumed) }

func (r Report) count(o Outcome) int {
	n := 0
	for _, res := range r.Results {
		if res.Outcome == o {
			n++
		}
	}
	return n
}

// Pass is the consolidation pass over one memory store.
type Pass struct {
	store       state.MemoryStore
	distiller   Distiller
	lessonKind  string
	episodeKind []string
	minEpisodes int
}

// Option configures a Pass.
type Option func(*Pass)

// WithLessonKind sets the kind a distilled lesson is written under. It must be a
// kind the write policy treats as replaceable or appendable to taste; the pass
// itself only ever writes one lesson per series. An empty kind is ignored.
func WithLessonKind(kind string) Option {
	return func(p *Pass) {
		if kind != "" {
			p.lessonKind = kind
		}
	}
}

// WithEpisodeKinds sets which kinds the pass reads as series members. A host that
// records "incident" and "attempt" alongside episodes consolidates all three by
// naming them here. An empty list is ignored, since a pass that read no kinds
// would sweep the store and do nothing.
func WithEpisodeKinds(kinds ...string) Option {
	return func(p *Pass) {
		if len(kinds) > 0 {
			p.episodeKind = slices.Clone(kinds)
		}
	}
}

// WithMinEpisodes sets how many episodes a subject needs before it is
// consolidated. Below 2 is treated as 2: distilling a single episode is not
// consolidation, it is rewriting one memory into another and losing the original.
func WithMinEpisodes(n int) Option {
	return func(p *Pass) {
		p.minEpisodes = max(n, 2)
	}
}

// New returns a consolidation pass over store, distilling through d. A nil store
// or distiller is ErrNoDistiller rather than a Pass that fails later.
func New(store state.MemoryStore, d Distiller, opts ...Option) (*Pass, error) {
	if store == nil || d == nil {
		return nil, ErrNoDistiller
	}
	p := &Pass{
		store:       store,
		distiller:   d,
		lessonKind:  KindLesson,
		episodeKind: []string{KindEpisode},
		minEpisodes: defaultMinEpisodes,
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Run consolidates every subject with a ready series in the scope q names, and
// reports what it did. The query is a RecallQuery so a caller can bound the sweep
// the way it bounds any other read - one scope, a time window, a subject list -
// rather than this package inventing a second way to say the same things.
//
// The kind filter is the pass's own and overrides whatever the query carried:
// a sweep is defined by the series it reads, and honouring a caller's kinds here
// would let a query that asked for facts consolidate them into lessons.
//
// Subjects are consolidated in a stable order (subject, then scope) so two runs
// over the same store do the same work in the same sequence, which is what makes
// a partial run's report worth reading.
func (p *Pass) Run(ctx context.Context, q state.RecallQuery) (Report, error) {
	q.Kinds = slices.Clone(p.episodeKind)
	items, err := p.store.Recall(ctx, q)
	if err != nil {
		return Report{}, fmt.Errorf("consolidate: read the series: %w", err)
	}

	var rep Report
	for _, g := range groupSeries(items) {
		res, err := p.consolidate(ctx, g)
		if err != nil {
			rep.Failures = append(rep.Failures, Failure{Subject: g.Subject, Scope: g.Scope, Err: err})
			continue
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

// Subject consolidates one subject in one scope, the call a host makes when it
// knows which series just grew rather than sweeping the store. The subject is
// normalized first, so a caller may pass whatever spelling it has in hand.
func (p *Pass) Subject(ctx context.Context, subject string, scope state.Scope) (Result, error) {
	key, err := state.NormalizeSubject(subject)
	if err != nil {
		return Result{}, err
	}
	if key == "" {
		// The unsubjected items are not a series. They are every observation nobody
		// filed under anything, and distilling them together would invent a topic.
		return Result{}, fmt.Errorf("%w: consolidation needs a subject", state.ErrInvalid)
	}
	items, err := p.store.Recall(ctx, state.RecallQuery{
		Subjects: []string{key}, Scope: scope, Kinds: slices.Clone(p.episodeKind),
	})
	if err != nil {
		return Result{}, fmt.Errorf("consolidate: read the series for %s: %w", key, err)
	}
	series := Series{Subject: key, Scope: scope}
	for _, it := range items {
		if it.Scope == scope {
			series.Episodes = append(series.Episodes, it)
		}
	}
	sortOldestFirst(series.Episodes)
	return p.consolidate(ctx, series)
}

// consolidate applies the pass to one series.
func (p *Pass) consolidate(ctx context.Context, s Series) (Result, error) {
	res := Result{Subject: s.Subject, Scope: s.Scope, Episodes: len(s.Episodes)}

	// An interrupted run is finished before anything new is distilled, so the
	// second run of a killed pass retires what the first one had already accounted
	// for instead of drawing a second lesson from the same series.
	lesson, covered, err := p.interrupted(ctx, s)
	if err != nil {
		return Result{}, err
	}
	if len(covered) > 0 {
		res.Outcome, res.Lesson = OutcomeResumed, lesson
		res.Retired, err = p.retire(ctx, covered)
		return res, err
	}

	if len(s.Episodes) < p.minEpisodes {
		res.Outcome = OutcomeTooFew
		return res, nil
	}
	out, err := p.distiller.Distil(ctx, s)
	if err != nil {
		return Result{}, fmt.Errorf("distil %s: %w", s.Subject, err)
	}
	if out.Content == "" {
		res.Outcome = OutcomeDeclined
		return res, nil
	}

	written, err := p.store.Write(ctx, p.lessonFrom(s, out))
	if err != nil {
		return Result{}, fmt.Errorf("write the lesson for %s: %w", s.Subject, err)
	}
	res.Outcome, res.Lesson = OutcomeDistilled, written
	res.Retired, err = p.retire(ctx, s.Episodes)
	return res, err
}

// lessonFrom builds the lesson item: the distiller's content, and everything else
// derived from the series it came from.
//
// Provenance is the union of the episodes' sources, so a purge that follows one
// compromised source still finds the lesson drawn from it. Taint spreads: a
// lesson distilled from one tainted episode is tainted, whatever the other four
// were, because the tainted content is in what the model read. Anchors are the
// union too, so the lesson rides along on every ref its episodes were about.
func (p *Pass) lessonFrom(s Series, out Lesson) state.MemoryItem {
	it := state.MemoryItem{
		Kind:    p.lessonKind,
		Subject: s.Subject,
		Scope:   s.Scope,
		Content: out.Content,
	}
	var anchors []state.Anchor
	for _, ep := range s.Episodes {
		it.Supersedes = append(it.Supersedes, ep.ID)
		it.Sources = appendMissing(it.Sources, ep.Sources...)
		anchors = append(anchors, ep.Anchors...)
		it.Tainted = it.Tainted || ep.Tainted
	}
	it.Anchors = anchors
	it.ExpiresAt = sharedExpiry(s.Episodes)
	return it
}

// sharedExpiry returns the latest expiry among the episodes when every one of
// them has one, and the zero time otherwise.
//
// A lesson must not outlive the facts it was drawn from, and must not die before
// them either. If every episode is due to expire, the lesson is only about
// material that will be gone, so it goes when the last of them does. If even one
// episode never expires, the lesson is about something durable and inherits no
// death date.
func sharedExpiry(episodes []state.MemoryItem) time.Time {
	var latest time.Time
	for _, ep := range episodes {
		if ep.ExpiresAt.IsZero() {
			return time.Time{}
		}
		if ep.ExpiresAt.After(latest) {
			latest = ep.ExpiresAt
		}
	}
	return latest
}

// interrupted reports the lesson an earlier run had already written for this
// series, and the episodes it named that are still live. It is what makes a
// killed run safe to repeat.
//
// A lesson only counts as this series' if it supersedes at least one episode in
// hand. Episodes written since that run are not covered by it and are left for
// the next one, so a resumed subject finishes exactly the work the interrupted
// run had accounted for and invents none.
func (p *Pass) interrupted(ctx context.Context, s Series) (state.MemoryItem, []state.MemoryItem, error) {
	if len(s.Episodes) == 0 {
		return state.MemoryItem{}, nil, nil
	}
	lessons, err := p.store.Recall(ctx, state.RecallQuery{
		Subjects: []string{s.Subject}, Scope: s.Scope, Kinds: []string{p.lessonKind},
	})
	if err != nil {
		return state.MemoryItem{}, nil, fmt.Errorf("consolidate: read the lessons on %s: %w", s.Subject, err)
	}
	for _, lesson := range lessons {
		if lesson.Scope != s.Scope || len(lesson.Supersedes) == 0 {
			continue
		}
		var covered []state.MemoryItem
		for _, ep := range s.Episodes {
			if slices.Contains(lesson.Supersedes, ep.ID) {
				covered = append(covered, ep)
			}
		}
		if len(covered) > 0 {
			return lesson, covered, nil
		}
	}
	return state.MemoryItem{}, nil, nil
}

// retire tombstones the episodes a lesson has taken over, returning the ids it
// retired. An episode that is already gone is not a failure: another run, or a
// host clearing up by hand, has done the same work, and the pass is defined by
// the state it leaves rather than by who got there first.
func (p *Pass) retire(ctx context.Context, episodes []state.MemoryItem) ([]string, error) {
	retired := make([]string, 0, len(episodes))
	for _, ep := range episodes {
		switch err := p.store.Delete(ctx, ep.ID); {
		case err == nil:
			retired = append(retired, ep.ID)
		case errors.Is(err, state.ErrNotFound):
		default:
			return retired, fmt.Errorf("retire episode %s: %w", ep.ID, err)
		}
	}
	return retired, nil
}

// groupSeries buckets recalled items by subject and scope, oldest first within a
// series, in a stable order across runs. Unsubjected items are dropped: they are
// not a series, and grouping them would distil a lesson about nothing in
// particular.
func groupSeries(items []state.MemoryItem) []Series {
	byKey := make(map[string]*Series)
	for _, it := range items {
		if it.Subject == "" {
			continue
		}
		key := it.Subject + "\x00" + it.Scope.Instance + "\x00" + it.Scope.Project + "\x00" + it.Scope.Workspace
		s, ok := byKey[key]
		if !ok {
			s = &Series{Subject: it.Subject, Scope: it.Scope}
			byKey[key] = s
		}
		s.Episodes = append(s.Episodes, it)
	}
	out := make([]Series, 0, len(byKey))
	for _, s := range byKey {
		sortOldestFirst(s.Episodes)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].Scope.Depth() > out[j].Scope.Depth()
	})
	return out
}

// sortOldestFirst puts a series into narrative order, with the id as the final
// tiebreak so two episodes stamped on one clock tick still order the same way on
// every run.
func sortOldestFirst(items []state.MemoryItem) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

// appendMissing appends the values not already present, preserving the order they
// were first credited in. Provenance order is the writer's credit, so the union
// keeps first-seen order rather than sorting it into something nobody asserted.
func appendMissing(dst []string, add ...string) []string {
	for _, v := range add {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
