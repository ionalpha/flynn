package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/ionalpha/flynn/inference/modelsource"
)

// requireConsent is the no-footgun gate: before a risky model run proceeds, the user must
// have explicitly accepted it. A trusted catalog model is not risky and runs with no
// prompt. For anything else (an unverified or unrecognized source), the risk is shown in
// plain language and consent is required: an explicit pre-approval grants it and is
// logged, an interactive session is prompted with the safe default being "no", and a
// non-interactive session is refused rather than assumed to consent. The safe answer is
// always the default, so a user cannot accidentally run a model they did not mean to.
func requireConsent(rs modelsource.RiskSurface, interactive, autoApprove bool, in io.Reader, out io.Writer) error {
	if !rs.Risky() {
		return nil
	}

	if autoApprove {
		// An explicit opt-in is honored, and recorded so the deliberate choice is on the
		// record rather than silent.
		_, _ = fmt.Fprintf(out, "proceeding: explicit consent (--yes) recorded for this %s source.\n", rs.Trust)
		return nil
	}
	if !interactive {
		return fmt.Errorf("refusing to run a %s-source model without consent in a non-interactive session; re-run interactively to confirm, or pass --yes to consent explicitly", rs.Trust)
	}

	_, _ = fmt.Fprintf(out, "run this %s model? [y/N]: ", rs.Trust)
	reader := bufio.NewReader(in)
	answer, _ := reader.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("declined: not running %s", rs.Source)
	}
}

// printRiskSurface writes the plain-language risk lines for a source, the surface shown
// wherever a model is selected or run so the trust, isolation, integrity, and network
// posture are visible without reading any documentation.
func printRiskSurface(out io.Writer, rs modelsource.RiskSurface) {
	for _, line := range rs.Lines() {
		_, _ = fmt.Fprintln(out, line)
	}
}
