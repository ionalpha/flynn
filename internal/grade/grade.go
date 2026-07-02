// Package grade scores how far an agent got toward an objective, not just whether
// it finished. An objective is expressed as a Ladder of ordered Milestones, each a
// deterministic ground-truth Check; grading awards partial credit for the rungs the
// agent actually reached. This turns a binary pass/fail into a dense signal: a run
// that finds and reproduces a fault but does not exploit it scores meaningfully
// above one that does nothing, which is what makes graded runs useful to learn
// from and to compare.
//
// Checks evaluate against Evidence, the recorded effects of a run (its spine
// events) plus the answers it gave to posed questions. Grading itself is pure: the
// same evidence grades identically on every machine and under replay, so a recorded
// run can be re-graded for free when the grader improves.
package grade

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/spine"
)

// Evidence is what a Check reasons over: the ground-truth record of a run. Events
// are the effects already durable on the spine (files touched, exit codes, results),
// and Answer looks up the run's response to a posed question by key. Grading never
// reads the agent's narration, only this recorded truth.
type Evidence interface {
	// Events returns the recorded events for the run under grading.
	Events() []spine.Event
	// Answer returns the run's answer to the question posed under key, and whether
	// one was given.
	Answer(key string) (string, bool)
}

// MapEvidence is a ready Evidence built from a slice of events and a map of posed
// answers, for tests and for adapting an in-memory run.
type MapEvidence struct {
	Recorded []spine.Event
	Answers  map[string]string
}

// Events returns the recorded events.
func (m MapEvidence) Events() []spine.Event { return m.Recorded }

// Answer returns the answer posed under key, if any.
func (m MapEvidence) Answer(key string) (string, bool) {
	v, ok := m.Answers[key]
	return v, ok
}

// Check is a deterministic predicate over evidence: did this milestone's
// ground-truth condition hold? It returns the outcome and a short human-readable
// detail. A Check must not consult a clock or randomness, so a grade is reproducible.
type Check func(ctx context.Context, ev Evidence) (passed bool, detail string, err error)

// Milestone is one rung of a ladder: a named condition worth Weight toward the
// total, satisfied when its Check passes. Weight defaults to 1 when zero; a negative
// weight is treated as 0.
type Milestone struct {
	Name   string
	Weight float64
	Check  Check
}

// Mode selects how a ladder accumulates credit.
type Mode int

const (
	// Cumulative awards credit for the leading run of passed rungs and stops at the
	// first failure: the classic progress ladder (find, then reproduce, then exploit)
	// where a later rung is meaningless without the earlier ones. Rungs after the
	// first failure are not evaluated.
	Cumulative Mode = iota
	// Independent evaluates and scores every rung on its own, awarding credit for any
	// that pass regardless of order.
	Independent
)

// Ladder is an ordered set of milestones graded under a Mode.
type Ladder struct {
	Name       string
	Mode       Mode
	Milestones []Milestone
}

// Rung is one milestone's graded outcome. Reached reports whether the rung was
// evaluated (always true in Independent mode; false for rungs past the first failure
// in Cumulative mode).
type Rung struct {
	Name    string
	Weight  float64
	Passed  bool
	Reached bool
	Detail  string
}

// Grade is a ladder's partial-credit result: the per-rung outcomes, the attained and
// total weight, the normalized Score in [0,1], and Reached, the number of passed
// rungs (in Cumulative mode this is the highest rung reached).
type Grade struct {
	Ladder   string
	Mode     Mode
	Rungs    []Rung
	Attained float64
	Total    float64
	Score    float64
	Reached  int
}

// Grade evaluates the ladder against ev and returns the partial-credit result. In
// Cumulative mode it stops crediting at the first failed rung and leaves the rest
// unreached; in Independent mode it scores every rung. An empty ladder scores 0 over
// a total of 0. A Check error aborts grading and is returned.
func (l Ladder) Grade(ctx context.Context, ev Evidence) (Grade, error) {
	g := Grade{Ladder: l.Name, Mode: l.Mode, Rungs: make([]Rung, 0, len(l.Milestones))}
	stopped := false
	for i, m := range l.Milestones {
		w := effectiveWeight(m.Weight)
		g.Total += w
		rung := Rung{Name: m.Name, Weight: w}
		if l.Mode == Cumulative && stopped {
			g.Rungs = append(g.Rungs, rung)
			continue
		}
		passed, detail, err := evaluate(ctx, m.Check, ev)
		if err != nil {
			return Grade{}, fmt.Errorf("grade: milestone %q (index %d): %w", m.Name, i, err)
		}
		rung.Reached = true
		rung.Passed = passed
		rung.Detail = detail
		switch {
		case passed:
			g.Attained += w
			g.Reached++
		case l.Mode == Cumulative:
			stopped = true
		}
		g.Rungs = append(g.Rungs, rung)
	}
	if g.Total > 0 {
		g.Score = g.Attained / g.Total
	}
	return g, nil
}

func evaluate(ctx context.Context, c Check, ev Evidence) (bool, string, error) {
	if c == nil {
		return false, "no check defined", nil
	}
	return c(ctx, ev)
}

func effectiveWeight(w float64) float64 {
	switch {
	case w == 0:
		return 1
	case w < 0:
		return 0
	default:
		return w
	}
}

// EventRecorded passes when the evidence holds a recorded event of eventType for
// which match returns true; a nil match accepts any event of that type. It is the
// ground-truth check: a milestone counts only when the effect is on the spine, not
// when the agent claims it happened.
func EventRecorded(eventType string, match func(payload map[string]any) bool) Check {
	return func(_ context.Context, ev Evidence) (bool, string, error) {
		for _, e := range ev.Events() {
			if e.Type != eventType {
				continue
			}
			if match == nil || match(e.Payload) {
				return true, "recorded " + eventType, nil
			}
		}
		return false, "no recorded " + eventType, nil
	}
}

// AnswerEquals passes when the run's answer for key equals want after trimming
// surrounding whitespace and folding case. It is the posed-question primitive: ask a
// question whose ground-truth answer is known (where the fault is, the final token)
// and verify the response against it.
func AnswerEquals(key, want string) Check {
	return func(_ context.Context, ev Evidence) (bool, string, error) {
		got, ok := ev.Answer(key)
		if !ok {
			return false, "no answer for " + key, nil
		}
		if normalize(got) == normalize(want) {
			return true, "answer matches", nil
		}
		return false, "answer mismatch", nil
	}
}

// AnswerMatches passes when the run's answer for key satisfies match. Use it when an
// answer is not a single canonical string: several acceptable forms, or a pattern.
func AnswerMatches(key string, match func(answer string) bool) Check {
	return func(_ context.Context, ev Evidence) (bool, string, error) {
		got, ok := ev.Answer(key)
		if !ok {
			return false, "no answer for " + key, nil
		}
		if match != nil && match(got) {
			return true, "answer accepted", nil
		}
		return false, "answer rejected", nil
	}
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
