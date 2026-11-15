package externagent

import "strings"

// promptLayers is the run's half of the prompt, in the order the harness reads it. The
// CLI's own harness prompt outranks it, then the workspace's conventions file, and the
// run's layers arrive last, inside the user turn. A conflict with the harness prompt
// therefore resolves against the run, so the run states its contract plainly, puts the
// checkable parts first, and lets the probes report whether the harness followed them.
type promptLayers struct {
	// system is the run's standing instruction, demoted to a preamble on the user turn.
	system string
	// probes are instructions whose compliance is checked against the event stream
	// after the episode.
	probes []string
	// input is the turn itself: the objective and its definition of done.
	input string
}

// render assembles the layers into the single string the CLI reads as the user turn.
// The probes come after the standing instruction, which frames them, and before the
// objective, because instruction-following decays under a long objective.
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
