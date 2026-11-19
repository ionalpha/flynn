package skillmd

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// MetaTriggers is the trigger list, a JSON array of [Trigger] objects. A trigger names
// something the run does that makes the host load this skill's body into the next turn,
// without the model asking for it.
const MetaTriggers = MetadataPrefix + "triggers"

// Trigger kinds. A reader that meets a kind it does not know skips that entry, so a
// later release can add a kind without making older hosts reject the skill.
const (
	// TriggerFile fires when a tool call's `path` argument matches Match, a [path.Match]
	// glob tried against the whole path and against its base name.
	TriggerFile = "file"
	// TriggerTool fires when a tool named Match is called.
	TriggerTool = "tool"
	// TriggerRepeat fires when the same tool is called with the same `command` argument
	// Count times in one run. Match narrows it to one tool and may be empty.
	TriggerRepeat = "repeat"
	// TriggerSlow fires when one tool call takes at least Seconds. Match narrows it to
	// one tool and may be empty.
	TriggerSlow = "slow"
)

// Bounds on a trigger list. They keep a hostile pack from making the per-call match
// expensive, and keep a count that could never be reached out of the manifest.
const (
	MaxTriggers     = 8
	MinRepeatCount  = 2
	MaxRepeatCount  = 20
	MaxSlowSeconds  = 3600
	maxTriggerMatch = 256
)

// Trigger is one declared reason to load a skill. Which fields apply depends on Kind;
// the others stay zero.
type Trigger struct {
	Kind    string `json:"kind"`
	Match   string `json:"match,omitempty"`
	Count   int    `json:"count,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

// EncodeTriggers renders triggers as the metadata value for [MetaTriggers]. An empty
// list encodes as "", which a writer omits.
func EncodeTriggers(ts []Trigger) string {
	if len(ts) == 0 {
		return ""
	}
	// Marshalling plain structs cannot fail.
	b, _ := json.Marshal(ts)
	return string(b)
}

// DecodeTriggers reads a value written by [EncodeTriggers]. Entries of a kind this
// release does not know are dropped, so a newer pack still loads. An entry of a known
// kind with a missing or out-of-range field is [ErrInvalid], because a trigger that
// silently never fires is worse than a refused pack.
func DecodeTriggers(v string) ([]Trigger, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fmt.Errorf("%w: triggers %q are not a JSON array: %w", ErrInvalid, v, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: triggers are null, want a JSON array", ErrInvalid)
	}
	if len(raw) > MaxTriggers {
		return nil, fmt.Errorf("%w: %d triggers, want at most %d", ErrInvalid, len(raw), MaxTriggers)
	}
	var out []Trigger
	for i, r := range raw {
		// A permissive decode: a field a later release adds must not fail this one.
		var t Trigger
		if err := json.Unmarshal(r, &t); err != nil {
			return nil, fmt.Errorf("%w: trigger %d is not an object: %w", ErrInvalid, i, err)
		}
		if !knownTriggerKind(t.Kind) {
			continue
		}
		if err := t.validate(); err != nil {
			return nil, fmt.Errorf("%w: trigger %d: %w", ErrInvalid, i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func knownTriggerKind(k string) bool {
	switch k {
	case TriggerFile, TriggerTool, TriggerRepeat, TriggerSlow:
		return true
	}
	return false
}

func (t Trigger) validate() error {
	if len(t.Match) > maxTriggerMatch {
		return fmt.Errorf("match is %d bytes, want at most %d", len(t.Match), maxTriggerMatch)
	}
	switch t.Kind {
	case TriggerFile:
		if t.Match == "" {
			return fmt.Errorf("file trigger needs a match glob")
		}
		if _, err := path.Match(t.Match, ""); err != nil {
			return fmt.Errorf("file glob %q: %w", t.Match, err)
		}
	case TriggerTool:
		if strings.TrimSpace(t.Match) == "" {
			return fmt.Errorf("tool trigger needs a tool name in match")
		}
	case TriggerRepeat:
		if t.Count < MinRepeatCount || t.Count > MaxRepeatCount {
			return fmt.Errorf("repeat count %d, want %d to %d", t.Count, MinRepeatCount, MaxRepeatCount)
		}
	case TriggerSlow:
		if t.Seconds < 1 || t.Seconds > MaxSlowSeconds {
			return fmt.Errorf("slow seconds %d, want 1 to %d", t.Seconds, MaxSlowSeconds)
		}
	}
	return nil
}

// CheckTriggers validates the triggers metadata of a document, so a pack with a
// malformed list is refused at publish time and not only when it is imported.
func CheckTriggers(meta map[string]string) error {
	raw, ok := Get(meta, MetaTriggers)
	if !ok {
		return nil
	}
	_, err := DecodeTriggers(raw)
	return err
}
