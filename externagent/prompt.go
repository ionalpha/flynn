package externagent

import "strings"

// The prompt an external agent CLI actually runs under has layers the run does not
// control. This file is the one place that maps the run's layers onto what the CLI
// accepts, so the demotion of the run's authority is explicit and testable rather than
// scattered across adapters.
//
// Authority, highest first:
//
//  1. The CLI's own harness prompt. Baked into the CLI, not settable by the run, and it
//     outranks everything below. It defines the agent the CLI believes it is.
//  2. The CLI's project conventions file, if the workspace has one. Also not the run's.
//  3. Everything the run injects, which the CLI receives as part of the user turn: the
//     run's standing instruction and its conformance probes.
//
// So the run's instructions arrive as a request from the user, not as a system rule, and
// a conflict with layer 1 resolves against the run. Nothing here can change that. What it
// can do is state the contract clearly, put the checkable parts first, and let the probes
// report when the harness ignored them. That is the honest position: the run steers, and
// measures whether the steering took.

// promptLayers is the run's half of the prompt, in the order the harness reads it.
type promptLayers struct {
	// system is the run's standing instruction, demoted to a preamble on the user turn.
	system string
	// probes are conformance instructions whose compliance is checked against the event
	// stream after the episode runs.
	probes []string
	// input is the actual turn: the objective and its definition of done.
	input string
}

// render assembles the layers into the single string the CLI reads as the user turn.
// The probes come after the standing instruction and before the objective: late enough
// that the standing instruction frames them, early enough that they are not buried under
// a long objective, which is where instruction-following decays.
func (p promptLayers) render() string {
	var parts []string
	if p.system != "" {
		parts = append(parts, p.system)
	}
	if len(p.probes) > 0 {
		parts = append(parts, strings.Join(p.probes, "\n"))
	}
	if p.input != "" {
		parts = append(parts, p.input)
	}
	return strings.Join(parts, "\n\n")
}
