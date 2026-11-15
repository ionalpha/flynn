// Package grade scores how far an agent got toward an objective. An objective is a
// Ladder of ordered Milestones, each a deterministic ground-truth Check, and grading
// awards partial credit for the rungs reached. A run that finds and reproduces a
// fault without exploiting it scores above one that does nothing, so graded runs
// can be compared and learned from.
//
// Checks evaluate Evidence: a run's spine events plus its answers to posed
// questions. Grading is pure, so the same evidence grades identically on every
// machine and under replay, and a recorded run can be re-graded when the grader
// improves.
package grade

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/spine"
)

// Evidence is the ground-truth record of a run a Check reads: the effects durable on
// the spine (files touched, exit codes, results) and the answers to posed questions.
// Grading never reads the agent's narration.
type Evidence interface {
	// Events returns the recorded events for the run under grading.
	Events() []spine.Event
	// Answer returns the run's answer to the question posed under key, and whether
	// one was given.
	Answer(key string) (string, bool)
}

// MapEvidence is an Evidence built from events and a map of answers, for tests and
// in-memory runs.
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

// Check is a deterministic predicate over evidence, returning the outcome and a
// short detail. It must not consult a clock or randomness, so a grade reproduces.
type Check func(ctx context.Context, ev Evidence) (passed bool, detail string, err error)

// Milestone is one rung of a ladder, worth Weight when its Check passes. A zero
// Weight counts as 1 and a negative one as 0.
type Milestone struct {
	Name   string
	Weight float64
	Check  Check
}

// Mode selects how a ladder accumulates credit.
type Mode int

const (
	// Cumulative credits the leading run of passed rungs and stops at the first
	// failure, for a progress ladder (find, reproduce, exploit) where a later rung
	// means nothing without the earlier ones. Later rungs are not evaluated.
	Cumulative Mode = iota
	// Independent scores every rung on its own, in any order.
	Independent
)

// Ladder is an ordered set of milestones graded under a Mode.
type Ladder struct {
	Name       string
	Mode       Mode
	Milestones []Milestone
}

// Rung is one milestone's outcome. Reached reports whether it was evaluated: always
// in Independent mode, never past the first failure in Cumulative mode.
type Rung struct {
	Name    string
	Weight  float64
	Passed  bool
	Reached bool
	Detail  string
}

// Grade is a ladder's partial-credit result: per-rung outcomes, attained and total
// weight, Score in [0,1], and Reached, the count of passed rungs (in Cumulative mode
// the highest rung reached).
type Grade struct {
	Ladder   string
	Mode     Mode
	Rungs    []Rung
	Attained float64
	Total    float64
	Score    float64
	Reached  int
}

// Grade evaluates the ladder against ev. An empty ladder scores 0 over a total of 0.
// A Check error aborts grading and is returned.
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

// EventRecorded passes when the evidence holds an event of eventType for which match
// returns true; a nil match accepts any. A milestone counts only when its effect is
// on the spine, whatever the agent claims.
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

// AnswerEquals passes when the answer for key equals want, trimmed and
// case-folded. It verifies a posed question with a known answer (where the fault
// is, the final token).
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

// AnswerMatches passes when the answer for key satisfies match, for answers with
// several acceptable forms or a pattern.
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
