package skilltool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/skillload"
	"github.com/ionalpha/flynn/state"
)

// toolRead is the name of the tool that loads a body at the model's request.
const toolRead = "skill_read"

var _ mission.Briefer = readTool{}

// WithStart names skills the host loads into the run's first turn, in the order given,
// without the model asking. A name that is unknown, has no readable body or fails the
// [WithStartCheck] is skipped and recorded as skipped; the rest still load.
func WithStart(slugs ...string) Option {
	return func(s *Set) { s.start = append(s.start[:0:0], slugs...) }
}

// WithStartCheck decides whether a skill named by [WithStart] can run in this run, and
// why not when it cannot. The host passes a check that reads the skill's required tools
// against the tools it mounted, so a procedure that needs a shell is not loaded into a
// run that has none. The default accepts every skill.
func WithStartCheck(check func(state.Skill) (ok bool, why string)) Option {
	return func(s *Set) { s.startCheck = check }
}

// WithLoadPolicy bounds the bodies one run loads by trigger. The zero policy allows
// [skillload.DefaultMaxTriggered].
func WithLoadPolicy(p skillload.Policy) Option {
	return func(s *Set) { s.policy = p }
}

// Loaded returns the ids of the skills the host loaded into this run, at start or by
// trigger, deduped, in load order. It is separate from [Set.Reads] on purpose: a body the
// model never asked for is no evidence that the skill helped, so it must not earn the
// skill credit in learn.Reinforce.
func (s *Set) Loaded() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return dedupe(s.loaded)
}

func (s *Set) recordLoaded(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = append(s.loaded, id)
}

// Brief loads bodies for the executor. At the start it loads the skills named for the
// run; after each turn it feeds the settled calls to [skillload.Decide] and loads what
// the declared triggers select. The state it returns carries what was loaded and counted,
// so a resumed run neither repeats a body nor resets a counter.
func (t readTool) Brief(ctx context.Context, in mission.BriefInput) (mission.BriefOutput, error) {
	return t.s.brief(ctx, in)
}

func (s *Set) brief(ctx context.Context, in mission.BriefInput) (mission.BriefOutput, error) {
	var st skillload.State
	if len(in.State) > 0 {
		// A state that cannot be read cannot say what is already loaded, so nothing loads.
		if err := json.Unmarshal(in.State, &st); err != nil {
			return mission.BriefOutput{}, fmt.Errorf("skilltool: brief state: %w", err)
		}
	}
	var b briefing
	if in.Start {
		st = s.loadNamed(ctx, st, &b)
	} else {
		var err error
		if st, err = s.loadTriggered(ctx, st, in.Calls, &b); err != nil {
			return mission.BriefOutput{}, err
		}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return mission.BriefOutput{}, err
	}
	return mission.BriefOutput{Text: strings.Join(b.texts, "\n\n"), State: raw, Notes: b.notes}, nil
}

type briefing struct {
	texts []string
	notes []mission.BriefNote
}

func (b *briefing) skip(slug, why string) {
	b.notes = append(b.notes, mission.BriefNote{Name: slug, Via: "skipped", Reason: why})
}

// add renders sk into the turn under a header that says why the host sent it.
func (b *briefing) add(ctx context.Context, s *Set, sk state.Skill, via, reason string) bool {
	text, err := s.render(ctx, sk)
	if err != nil {
		b.skip(sk.Slug, err.Error())
		return false
	}
	s.recordLoaded(sk.ID)
	b.texts = append(b.texts, fmt.Sprintf("Skill %s, loaded for you (%s):\n\n%s", sk.Slug, reason, text))
	b.notes = append(b.notes, mission.BriefNote{Name: sk.Slug, Via: via, Reason: reason})
	return true
}

// loadNamed loads the start skills in the order the host named them.
func (s *Set) loadNamed(ctx context.Context, st skillload.State, b *briefing) skillload.State {
	for _, name := range s.start {
		sk, err := s.get(ctx, name)
		if err != nil {
			b.skip(name, err.Error())
			continue
		}
		if st.Has(sk.Slug) {
			continue
		}
		if s.startCheck != nil {
			if ok, why := s.startCheck(sk); !ok {
				b.skip(sk.Slug, why)
				continue
			}
		}
		if b.add(ctx, s, sk, "start", "named for this run") {
			st = st.MarkLoaded(sk.Slug)
		}
	}
	return st
}

// loadTriggered applies the declared triggers to the calls that just settled.
func (s *Set) loadTriggered(ctx context.Context, st skillload.State, calls []mission.ObservedCall, b *briefing) (skillload.State, error) {
	found, err := s.skills.Search(ctx, "", 0)
	if err != nil {
		return st, err
	}
	bySlug := make(map[string]state.Skill, len(found))
	var cands []skillload.Candidate
	for _, sk := range found {
		if _, dup := bySlug[sk.Slug]; dup || len(sk.Triggers) == 0 {
			continue
		}
		bySlug[sk.Slug] = sk
		cands = append(cands, skillload.Candidate{Slug: sk.Slug, Triggers: sk.Triggers})
	}

	obs := skillload.Observation{}
	for _, c := range calls {
		obs.Calls = append(obs.Calls, skillload.Call{Tool: c.Tool, Input: c.Input, Elapsed: c.Elapsed})
		if c.Tool != toolRead || c.IsError {
			continue
		}
		var in struct {
			Skill string `json:"skill"`
		}
		if json.Unmarshal(c.Input, &in) != nil {
			continue
		}
		if sk, err := s.get(ctx, in.Skill); err == nil {
			obs.Read = append(obs.Read, sk.Slug)
		}
	}

	d := skillload.Decide(s.policy, st, cands, obs)
	for _, l := range d.Loads {
		b.add(ctx, s, bySlug[l.Slug], "trigger", l.Reason)
	}
	for _, l := range d.Suppressed {
		b.notes = append(b.notes, mission.BriefNote{Name: l.Slug, Via: "suppressed", Reason: l.Reason})
	}
	return d.State, nil
}
