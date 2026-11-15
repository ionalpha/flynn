// Package consolidate distils a subject's episode series into one lesson. Episodes
// append (see memory/curate), so a failing deploy accumulates five near-identical
// items that a digest cannot afford to carry. The pass writes a lesson superseding
// them and tombstones the episodes.
//
// The host supplies the [Distiller], because summarizing needs a model. This
// package decides which series are ready, what the lesson inherits, what is retired
// and what a re-run does.
//
// # Running it again
//
// The pass is idempotent, because offline work can be killed halfway. The lesson is
// written before the episodes are retired, and a later run that finds a lesson
// naming live episodes finishes retiring them instead of distilling again.
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

// Default kinds for lessons written and episodes read; override with
// [WithLessonKind] and [WithEpisodeKinds].
const (
	KindLesson  = "lesson"
	KindEpisode = "episode"
)

// defaultMinEpisodes is 3, because a pair is short enough to read and not worth a
// model call.
const defaultMinEpisodes = 3

// Series is one subject's episodes, oldest first, because what changed between
// the first and last is most of a lesson.
type Series struct {
	Subject  string
	Scope    state.Scope
	Episodes []state.MemoryItem
}

// Lesson is a distiller's content only. The pass derives provenance, anchors, taint
// and expiry from the series, so a distiller cannot clean a tainted episode or drop
// the sources a purge follows.
type Lesson struct {
	// Content is the lesson. Empty declines the series: nothing is written or
	// retired, which costs only a re-read next run.
	Content string
}

// Distiller turns a series into a lesson. It may be called for many subjects in one
// run, so it owns its rate limiting and budget. An error fails that subject only.
type Distiller interface {
	Distil(ctx context.Context, in Series) (Lesson, error)
}

// DistillerFunc adapts a function to Distiller.
type DistillerFunc func(ctx context.Context, in Series) (Lesson, error)

// Distil calls f.
func (f DistillerFunc) Distil(ctx context.Context, in Series) (Lesson, error) { return f(ctx, in) }

// ErrNoDistiller is returned by [New] for a nil store or distiller, so a miswired
// host fails at startup and not on a nightly job.
var ErrNoDistiller = errors.New("consolidate: a distiller is required")

// Outcome is what the pass did with one subject.
type Outcome string

const (
	// OutcomeDistilled wrote a lesson and retired the episodes it was drawn from.
	OutcomeDistilled Outcome = "distilled"
	// OutcomeResumed finished retiring episodes an interrupted run's lesson covers.
	OutcomeResumed Outcome = "resumed"
	// OutcomeTooFew left the series alone: not enough episodes yet.
	OutcomeTooFew Outcome = "too-few"
	// OutcomeDeclined left the series alone: the distiller returned no content.
	OutcomeDeclined Outcome = "declined"
)

// Result is one subject's outcome in one run.
type Result struct {
	Subject string
	Scope   state.Scope
	Outcome Outcome
	// Episodes is how many episodes were in hand when the subject was considered.
	Episodes int
	// Lesson is the item written or resumed; zero when nothing was distilled.
	Lesson state.MemoryItem
	// Retired is the ids tombstoned, in order.
	Retired []string
}

// Failure is a subject the pass could not finish. A sweep collects these and
// continues, so one timeout does not cost every other subject.
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

// Distilled counts subjects given a new lesson, excluding resumed ones.
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

// WithLessonKind sets the lesson kind. An empty kind is ignored.
func WithLessonKind(kind string) Option {
	return func(p *Pass) {
		if kind != "" {
			p.lessonKind = kind
		}
	}
}

// WithEpisodeKinds sets the kinds read as series members. An empty list is ignored.
func WithEpisodeKinds(kinds ...string) Option {
	return func(p *Pass) {
		if len(kinds) > 0 {
			p.episodeKind = slices.Clone(kinds)
		}
	}
}

// WithMinEpisodes sets the episodes needed before distilling. Below 2 is 2, because
// distilling one episode would only replace it.
func WithMinEpisodes(n int) Option {
	return func(p *Pass) {
		p.minEpisodes = max(n, 2)
	}
}

// New returns a pass over store distilling through d.
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

// Run consolidates every ready series q selects. q.Kinds is replaced by the episode
// kinds, so a query for facts cannot turn them into lessons. Subjects run in a
// stable order, so two runs over one store do the same work in sequence.
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

// Subject consolidates one subject in exactly scope, for a host that knows which
// series grew. The subject is normalized first.
func (p *Pass) Subject(ctx context.Context, subject string, scope state.Scope) (Result, error) {
	key, err := state.NormalizeSubject(subject)
	if err != nil {
		return Result{}, err
	}
	if key == "" {
		// Unsubjected items share no topic, so they are not a series.
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

func (p *Pass) consolidate(ctx context.Context, s Series) (Result, error) {
	res := Result{Subject: s.Subject, Scope: s.Scope, Episodes: len(s.Episodes)}

	// Finish an interrupted run first, so the series is not distilled twice.
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

// lessonFrom builds the lesson item. Sources and anchors are the union of the
// episodes', so a purge of one source finds the lesson. One tainted episode taints
// the lesson, because the model read it.
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

// sharedExpiry returns the latest episode expiry, or zero if any episode never
// expires, so a lesson neither outlives nor predeceases its evidence.
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

// interrupted returns an earlier lesson on the series and the live episodes it
// supersedes. Episodes written since are not covered and wait for a later run.
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

// retire tombstones episodes and returns the ids retired. An episode already gone
// is not a failure, because the state is what was wanted.
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

// groupSeries buckets items by subject and scope in a stable order, dropping
// unsubjected items.
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

// sortOldestFirst breaks ties by ID, so episodes on one clock tick order the same
// way every run.
func sortOldestFirst(items []state.MemoryItem) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

// appendMissing appends values not present, keeping first-seen order because
// source order is the writer's credit.
func appendMissing(dst []string, add ...string) []string {
	for _, v := range add {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
