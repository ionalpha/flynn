package externagent

import "strings"

// Strip says how one class of an external CLI's native surface is taken away for an
// episode. It names the mechanism rather than the flag that expresses it, because the
// flag is the one part of a lockdown that is guaranteed not to transfer: the binary,
// the flag spelling and the tool names differ per provider, so a rule written as
// "pass --disallowedTools Edit Write" is a rule that holds on exactly one CLI and
// silently holds on none of the others.
type Strip int

const (
	// StripUndeclared is the zero value, and means the adapter said nothing about this
	// class. It is refused, not assumed safe: an adapter that forgot to state its posture
	// is indistinguishable from one that has none, and the whole point of the contract is
	// that the run does not have to trust the adapter author's memory.
	StripUndeclared Strip = iota
	// StripDenied means the CLI's own controls remove the tool from the model's surface,
	// so the model is never offered it. This is the stronger form: a denied tool costs no
	// turn and produces no refusal the model has to interpret.
	StripDenied
	// StripContained means the CLI keeps offering the tool and the containment boundary
	// makes it fail. The effect cannot land, but the attempt is the model's to make, so a
	// contained class shows up in the steering metrics as native drift.
	StripContained
	// StripImpossible means neither is available on this provider. It is a refusal, not a
	// downgrade: an episode whose writes cannot be taken away is not run, and a judgement
	// that would need one is not made.
	StripImpossible
)

// String names the posture for a refusal message and the record.
func (s Strip) String() string {
	switch s {
	case StripDenied:
		return "denied"
	case StripContained:
		return "contained"
	case StripImpossible:
		return "impossible"
	case StripUndeclared:
		return "undeclared"
	default:
		return "unknown"
	}
}

// Stripped reports whether the class is actually taken away, by either mechanism.
func (s Strip) Stripped() bool { return s == StripDenied || s == StripContained }

// Lockdown is one provider's statement of what an episode's harness is left holding:
// how its native writes and commands are taken away, and whether it runs on the
// operator's own configuration.
//
// It exists because the alternative is authorship. Both bundled adapters lock their CLI
// down correctly today, and both do it inside the argv their Command builds, where
// nothing outside the adapter can read the posture and nothing checks that it is still
// there. A third adapter that simply omits the lockdown produces episodes that look
// exactly like governed ones in the record. Stating the posture per provider makes the
// omission a refusal at the point the episode would start.
//
// The config half is not a smaller concern than the tool half. A CLI started under the
// operator's own home reads their MCP servers, hooks and plugins, so the harness is
// steerable by whatever configuration the person who launched the run happens to carry,
// and an episode's behavior stops being a function of the run. The contract therefore
// covers the environment as well as the toolset.
type Lockdown struct {
	// Writes is how the CLI's native file writes are taken away.
	Writes Strip
	// Commands is how the CLI's native shell and command execution is taken away.
	Commands Strip
	// HostConfig is how the operator's own CLI configuration (their MCP servers, hooks,
	// plugins and settings) is kept out of the episode. StripDenied is a CLI told to read
	// none of it; StripContained is a CLI pointed at a per-episode home holding only what
	// it needs to authenticate.
	HostConfig Strip
	// Reason states, in one line, why a class is impossible, and is required whenever one
	// is. It is what the refusal says out loud, so an operator meets an integration gap
	// rather than a broken run.
	Reason string
}

// Refusal reports why an episode must not run under this lockdown, or "" when it may.
// provider names the CLI in the message, since the operator's next move is to pick a
// different backend or to close the gap in this one.
//
// The message names every class that failed rather than the first, because an operator
// who fixes one and re-runs to meet the next has been told half the answer twice.
func (l Lockdown) Refusal(provider string) string {
	var missing []string
	for _, c := range []struct {
		name string
		s    Strip
	}{
		{"native writes", l.Writes},
		{"native commands", l.Commands},
		{"the operator's own configuration", l.HostConfig},
	} {
		if !c.s.Stripped() {
			missing = append(missing, c.name+" ("+c.s.String()+")")
		}
	}
	if len(missing) == 0 {
		return ""
	}
	msg := "refusing to run an episode on " + provider + ": it cannot be stripped of " + strings.Join(missing, ", ")
	if l.Reason != "" {
		msg += "; " + l.Reason
	}
	return msg
}
