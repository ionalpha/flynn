package externagent

import "strings"

// Strip says how one class of an external CLI's native surface is taken away for an
// episode. It names the mechanism instead of the flag, because the binary, the flag
// spelling and the tool names differ per provider, so a rule written as a flag holds
// on one CLI and on none of the others.
type Strip int

const (
	// StripUndeclared is the zero value: the adapter said nothing about this class. It
	// is refused, because an adapter that forgot its posture looks the same as one that
	// has none.
	StripUndeclared Strip = iota
	// StripDenied means the CLI's own controls remove the tool, so the model is never
	// offered it. A denied tool costs no turn and produces no refusal to interpret.
	StripDenied
	// StripContained means the CLI still offers the tool and the containment boundary
	// makes it fail. The attempt is still the model's to make, so a contained class
	// shows up in the steering metrics as native drift.
	StripContained
	// StripImpossible means neither mechanism is available on this provider. It is a
	// refusal: an episode whose writes cannot be taken away is not run.
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

// Stripped reports whether either mechanism takes the class away.
func (s Strip) Stripped() bool { return s == StripDenied || s == StripContained }

// Lockdown is one provider's statement of what an episode's harness is left holding:
// how its native writes and commands are taken away, and whether it runs on the
// operator's own configuration. The adapters build the lockdown into their argv, where
// nothing else can check it, so a third adapter that omitted it would produce episodes
// that look governed. Declaring the posture per provider makes the omission a refusal
// before the episode starts.
//
// A CLI started under the operator's home reads their MCP servers, hooks and plugins,
// so HostConfig is stripped as well as the tools; otherwise an episode's behavior would
// depend on the launcher's configuration.
type Lockdown struct {
	Writes Strip
	// Commands covers the CLI's native shell and command execution.
	Commands Strip
	// HostConfig is how the operator's own CLI configuration (MCP servers, hooks,
	// plugins, settings) is kept out. StripDenied is a CLI told to read none of it;
	// StripContained is a CLI pointed at a separate home holding only its credentials
	// and its own state.
	HostConfig Strip
	// Reason is the one-line explanation required whenever a class is impossible, so
	// the operator reads an integration gap instead of a broken run.
	Reason string
}

// Refusal reports why an episode must not run under this lockdown, or "" when it may.
// It names every class that failed, so an operator does not fix one, re-run, and meet
// the next.
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
