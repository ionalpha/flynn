package main

// The session's slash commands as the line interface runs them: dispatching the typed
// line, and the two commands whose behaviour the full-screen interface shares rather
// than reimplements (/models and /model).

import (
	"context"
	"fmt"
	"io"

	"github.com/ionalpha/flynn/harness"
)

// replCommand runs the line as one of the session's slash commands, when it names one.
// It reports whether it claimed the line and any error to surface, so each interface
// renders the outcome its own way. A line that is not a command is left for the model.
//
// What each command does is the line handler on its row of sessionCommands, beside the
// handler the full-screen interface runs for the same command; this is only the half
// that turns a typed line into one of them.
func (s *replSession) replCommand(ctx context.Context, line string) (handled bool, err error) {
	cmd, arg, ok := lookupCommand(line)
	if !ok {
		return false, nil
	}
	return true, cmd.line(s, ctx, arg)
}

// showCatalog prints the model catalog into the session, the same view as `flynn models`,
// so a user can see what to switch to without leaving the session. It writes to out so
// both front-ends can place it: line mode prints straight through, the full-screen
// session captures it into the scrollback.
func (s *replSession) showCatalog(out io.Writer) error {
	return runModels(nil, s.dataDir, out)
}

// switchModel changes the model the rest of the session drives. With no argument it
// reports the current model; otherwise it resolves the requested "provider:model" spec,
// swaps it in for the next turn, and records it as the default so a later launch reuses
// it. A spec that cannot be resolved (an unknown provider, a missing key) is reported
// without ending the session. Feedback goes to out so either front-end can place it.
func (s *replSession) switchModel(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(out, "  current model: %s\n  switch with: /model <provider:model> (see /models)\n", s.modelSpec)
		return nil
	}
	spec := args[0]
	name, cliModel, isExt := externalAgentSpec(spec)

	switch {
	case isExt && s.ext != nil && s.ext.driver.Name() == name:
		// Same harness, different model: the CLI keeps driving, and the next episode runs
		// the model named here. The conversation the CLI holds is not disturbed, which is
		// the point of switching rather than restarting.
		s.ext.model = cliModel

	case s.started && (isExt || s.ext != nil):
		// The run's record declares which harness drove it, and a record states one
		// provenance for the whole run. Swapping the harness mid-run would seal a record
		// whose declaration is true of only part of it, so the swap is refused rather than
		// quietly producing that. A new session is one line away.
		return fmt.Errorf("/model %s: this run is already being driven by %s, and a run's record declares the one harness that drove it. "+
			"Leave and start a new session to drive %s", spec, s.harnessName(), spec)

	case isExt:
		// Nothing has run yet, so the session is free to become an external one.
		ea, err := resolveExternalAgent(ctx, name, cliModel, s.cwd)
		if err != nil {
			return fmt.Errorf("/model %s: %w", spec, err)
		}
		s.ext.close() // the harness this session is no longer going to drive
		s.ext = ea
		s.model, s.plan = nil, harness.Plan{}
		// An external harness exposes no model to distill through, so a session it drives
		// does not learn back. The one-shot path skips it for the same reason.
		s.distiller = nil

	default:
		model, plan, err := resolveModel(ctx, spec, s.dataDir)
		if err != nil {
			return fmt.Errorf("/model %s: %w", spec, err)
		}
		s.model = model
		s.plan = plan
		s.ext.close() // switching to a native model before any turn ran
		s.ext = nil
		// A distilling session learns through the model, so keep the distiller on the model
		// the session now drives.
		if s.learnEnabled {
			s.distiller = governedDistiller(model)
		}
	}

	s.modelSpec = spec
	if err := writeActiveModel(s.dataDir, spec); err != nil {
		// The switch still holds for this session; only persistence failed.
		_, _ = fmt.Fprintf(out, "  switched to %s (could not save it as the default: %v)\n", spec, err)
		return nil
	}
	_, _ = fmt.Fprintf(out, "  switched to %s; saved as the default for the next run\n", spec)
	return nil
}

// harnessName names what is driving the session, for a message that has to say so.
func (s *replSession) harnessName() string {
	if s.ext != nil {
		return s.ext.driver.Name()
	}
	return s.modelSpec
}
