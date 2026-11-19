// Package skillload decides which skills the host loads into a run without the model
// asking, from what the run just did. It is pure: the caller supplies the candidate
// skills and the observed tool calls, and gets back the loads and the skips, so every
// rule is a table test and the executor owns no policy.
//
// The state a decision leaves behind serialises, because a run resumes from a
// checkpoint and must not load a body twice, exceed its cap or forget how many times a
// command ran just because the process restarted.
package skillload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// DefaultMaxTriggered caps the bodies one run loads by trigger. A body is a few
// thousand tokens, and past three the context is spent on instructions the model did
// not ask for; the descriptions stay on offer for it to read itself.
const DefaultMaxTriggered = 3

// maxTracked bounds the repeat counters, so a run issuing endless distinct commands
// cannot grow its checkpoint without limit.
const maxTracked = 128

// Candidate is a skill the host may load, with the triggers it declares.
type Candidate struct {
	Slug     string
	Triggers []state.SkillTrigger
}

// Call is one tool call the run made and the host timed.
type Call struct {
	Tool    string
	Input   json.RawMessage
	Elapsed time.Duration
}

// Observation is what happened since the last decision: the tool calls of one settled
// turn, in call order, and the skills the model itself read in that turn. A skill the
// model read is loaded, so a trigger never repeats it.
type Observation struct {
	Calls []Call
	Read  []string
}

// State is what the run has loaded and counted so far. The zero value is a fresh run.
type State struct {
	// Loaded holds every slug whose body is in the conversation, loaded by the model,
	// at start or by trigger.
	Loaded []string `json:"loaded,omitempty"`
	// Triggered counts bodies loaded by trigger, the number the cap bounds.
	Triggered int `json:"triggered,omitempty"`
	// Suppressed holds slugs whose trigger fired past the cap, so each is reported once.
	Suppressed []string `json:"suppressed,omitempty"`
	// Repeats counts calls by tool and command, keyed by [repeatKey].
	Repeats map[string]int `json:"repeats,omitempty"`
}

// Policy bounds trigger loading.
type Policy struct {
	// MaxTriggered is the most bodies one run loads by trigger; zero means
	// [DefaultMaxTriggered] and a negative value disables trigger loading.
	MaxTriggered int
}

func (p Policy) max() int {
	if p.MaxTriggered == 0 {
		return DefaultMaxTriggered
	}
	return p.MaxTriggered
}

// Load is a body to put in the next turn and the trigger that earned it.
type Load struct {
	Slug   string
	Reason string
}

// Decision is the outcome of one observation.
type Decision struct {
	State State
	// Loads are in candidate order, so the order a pack lists skills is the order
	// their bodies reach the model.
	Loads []Load
	// Suppressed are skills whose trigger fired but whose body was not loaded, with
	// the reason.
	Suppressed []Load
}

// Has reports whether slug is already loaded.
func (s State) Has(slug string) bool { return slices.Contains(s.Loaded, slug) }

// MarkLoaded returns s with slug recorded as loaded. It does not count against the
// cap: the model asked for it, or the run named it.
func (s State) MarkLoaded(slug string) State {
	if s.Has(slug) {
		return s
	}
	s.Loaded = append(slices.Clone(s.Loaded), slug)
	return s
}

// Decide applies the candidates' triggers to obs. It returns the new state and never
// changes the one passed in.
func Decide(p Policy, st State, cands []Candidate, obs Observation) Decision {
	st.Loaded = slices.Clone(st.Loaded)
	st.Suppressed = slices.Clone(st.Suppressed)
	for _, slug := range obs.Read {
		st = st.MarkLoaded(slug)
	}
	st.Repeats = countRepeats(st.Repeats, obs.Calls)

	d := Decision{}
	for _, c := range cands {
		if st.Has(c.Slug) {
			continue
		}
		reason, ok := firstFired(c.Triggers, st, obs)
		if !ok {
			continue
		}
		if p.max() < 0 || st.Triggered >= p.max() {
			if !slices.Contains(st.Suppressed, c.Slug) {
				st.Suppressed = append(st.Suppressed, c.Slug)
				d.Suppressed = append(d.Suppressed, Load{Slug: c.Slug, Reason: "cap reached; " + reason})
			}
			continue
		}
		st.Loaded = append(st.Loaded, c.Slug)
		st.Triggered++
		d.Loads = append(d.Loads, Load{Slug: c.Slug, Reason: reason})
	}
	d.State = st
	return d
}

// countRepeats returns a copy of counts with every call in calls added. Only calls
// with a command argument count, because a repeat trigger is about the same shell
// command run again and a file read twice is not a stuck loop.
func countRepeats(counts map[string]int, calls []Call) map[string]int {
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	for _, c := range calls {
		cmd := stringArg(c.Input, "command")
		if cmd == "" {
			continue
		}
		k := repeatKey(c.Tool, cmd)
		if _, seen := out[k]; !seen && len(out) >= maxTracked {
			continue
		}
		out[k]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// repeatKey hashes the command so the stored state stays small and holds no command
// text, which can carry a secret.
func repeatKey(tool, command string) string {
	sum := sha256.Sum256([]byte(command))
	return tool + ":" + hex.EncodeToString(sum[:6])
}

// firstFired returns the reason of the first declared trigger obs satisfies.
func firstFired(ts []state.SkillTrigger, st State, obs Observation) (string, bool) {
	for _, t := range ts {
		if r, ok := fires(t, st, obs); ok {
			return r, true
		}
	}
	return "", false
}

func fires(t state.SkillTrigger, st State, obs Observation) (string, bool) {
	for _, c := range obs.Calls {
		switch t.Kind {
		case skillmd.TriggerTool:
			if c.Tool == t.Match {
				return "called tool " + c.Tool, true
			}
		case skillmd.TriggerFile:
			if p := stringArg(c.Input, "path"); p != "" && MatchPath(t.Match, p) {
				return "touched " + p, true
			}
		case skillmd.TriggerRepeat:
			cmd := stringArg(c.Input, "command")
			if cmd == "" || (t.Match != "" && t.Match != c.Tool) {
				continue
			}
			if n := st.Repeats[repeatKey(c.Tool, cmd)]; n >= t.Count {
				return "ran the same command " + strconv.Itoa(n) + " times", true
			}
		case skillmd.TriggerSlow:
			if t.Match != "" && t.Match != c.Tool {
				continue
			}
			if c.Elapsed >= time.Duration(t.Seconds)*time.Second {
				return c.Tool + " took " + c.Elapsed.Round(time.Second).String(), true
			}
		}
	}
	return "", false
}

// MatchPath reports whether a [path.Match] glob matches p or any trailing run of its
// directories, so "go.mod" and "**/go.mod" both match "svc/api/go.mod". A leading
// "**/" is the only recursive form.
func MatchPath(glob, p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	glob = strings.TrimPrefix(glob, "**/")
	for {
		if ok, _ := path.Match(glob, p); ok {
			return true
		}
		i := strings.Index(p, "/")
		if i < 0 {
			return false
		}
		p = p[i+1:]
	}
}

func stringArg(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return s
}
