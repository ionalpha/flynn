package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/spine"
)

// projectFilesFlag is the value of --project-files, read by every entry point that
// starts a run. It is a package variable for the same reason as bundledSkillsDisabled:
// the setting is the invocation's, and threading it through each command's signature
// would touch every one of them for a value only this file reads.
var projectFilesFlag string

// projectFilesEnv names the environment variable that sets the mode when the flag is
// absent, so a CI job or a shell profile can say "trust" or "skip" once.
const projectFilesEnv = "FLYNN_PROJECT_FILES"

// projectTrustFile is the file under the data directory that remembers approved
// instruction files, keyed by repository and content.
const projectTrustFile = "project-trust.json"

// projectFilesMode resolves the mode from the flag, then the environment, then the
// default of asking.
func projectFilesMode() (projectctx.Mode, error) {
	v := projectFilesFlag
	if v == "" {
		v = os.Getenv(projectFilesEnv)
	}
	return projectctx.ParseMode(v)
}

// resolveProjectFiles finds the instruction files that apply to workdir and settles
// whether they load. A zero [projectctx.Decision] comes back when there are none, and
// callers treat it as nothing to add, taint or record. A question is put only when
// interactive is set, so a run with nobody to ask skips the files and says so.
//
// An error means the files could not be examined; the session still runs, without
// them, because a cloned repository must not be able to stop Flynn starting by
// being awkward to read.
func resolveProjectFiles(workdir, dataDir string, interactive bool, in io.Reader, out io.Writer) (projectctx.Decision, error) {
	mode, err := projectFilesMode()
	if err != nil {
		return projectctx.Decision{}, err
	}
	// Skip mode still looks, so the record can say which files a run was started
	// without; reading a file into memory to hash it is not acting on what it says.
	set, err := projectctx.Discover(workdir)
	if err != nil {
		return projectctx.Decision{}, fmt.Errorf("project files: %w", err)
	}
	if set.Empty() {
		return projectctx.Decision{}, nil
	}
	policy := projectctx.Policy{
		Mode:       mode,
		Remembered: projectctx.NewFileStore(filepath.Join(dataDir, projectTrustFile)),
	}
	if interactive {
		policy.Ask = terminalChooser(bufio.NewReader(in), out)
	}
	d := projectctx.Decide(set, policy)
	if d.RememberErr != nil {
		_, _ = fmt.Fprintf(out, "  (approval not remembered: %v)\n", d.RememberErr)
	}
	return d, nil
}

// terminalChooser asks at the terminal whether to load the files. Nothing but an
// explicit yes loads them: an empty answer, an unreadable one and end of input all
// decline.
func terminalChooser(in *bufio.Reader, out io.Writer) projectctx.Chooser {
	return func(set projectctx.Set) (projectctx.Choice, error) {
		for _, l := range describeSet(set) {
			_, _ = fmt.Fprintln(out, l)
		}
		_, _ = fmt.Fprint(out, "Load them? [y] this session, [a] always for this content, [N] no: ")
		answer, err := in.ReadString('\n')
		if err != nil && answer == "" {
			return projectctx.ChoiceNo, err
		}
		return parseTrustAnswer(answer), nil
	}
}

// parseTrustAnswer reads the answer to the trust question. Anything it does not recognise
// is a no.
func parseTrustAnswer(answer string) projectctx.Choice {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return projectctx.ChoiceOnce
	case "a", "always":
		return projectctx.ChoiceAlways
	default:
		return projectctx.ChoiceNo
	}
}

// describeSet is what the person deciding sees: each file with its size, anything
// the loader removed or flagged in it, and what was left out and why.
func describeSet(set projectctx.Set) []string {
	lines := []string{
		"",
		"This repository has project instruction files. Loading them adds their text to the model's instructions.",
		"They are written by whoever controls the repository, so they are treated as untrusted input:",
		"anything learned from a session that read them is kept out of the standing memory digest.",
	}
	for _, f := range set.Files {
		l := fmt.Sprintf("  %s (%d bytes)", f.Path, f.Bytes)
		if f.Truncated {
			l += " [cut at the size limit]"
		}
		if len(f.Findings) > 0 {
			l += " [hidden or instruction-like text found: " + strings.Join(f.Findings, ", ") + "]"
		}
		lines = append(lines, l)
	}
	for _, s := range set.Skipped {
		lines = append(lines, fmt.Sprintf("  not loaded: %s (%s)", s.Path, s.Reason))
	}
	return lines
}

// recordProjectDecision writes the decision to the run's stream. A decision with no
// files is not recorded. A failure is reported and does not stop the run: the
// decision was already acted on, and the run's record is the place to look for it,
// so its absence is worth saying.
func recordProjectDecision(ctx context.Context, out io.Writer, log spine.Log, d projectctx.Decision, runID string) {
	if d.Set.Empty() || runID == "" {
		return
	}
	if err := d.Append(ctx, log, runID); err != nil {
		_, _ = fmt.Fprintf(out, "  (project file decision not recorded: %v)\n", err)
	}
}

// withProjectFiles adds the resolved decision to a run: the loaded files go into the
// system prompt, the run is tainted by them, and the decision is recorded on the
// run's stream once the run has an id.
func withProjectFiles(d projectctx.Decision) driveOption {
	return func(c *driveConfig) { c.project = d }
}

// projectDecision resolves the decision for a session or run that is about to start.
// A failure to examine the files is reported on stderr and leaves the run without them.
func projectDecision(workdir, dataDir string) projectctx.Decision {
	d, err := resolveProjectFiles(workdir, dataDir, stdinIsTerminal(), os.Stdin, os.Stdout)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "warning:", err)
	}
	return d
}

// withProjectPrompt appends the fenced instruction files to a session's standing
// instructions when the session's decision loaded them.
func (s *replSession) withProjectPrompt(system string) string {
	if p := s.project.Prompt(); p != "" {
		return system + "\n\n" + p
	}
	return system
}

// sessionTaint is what the session's closing pass marks its taint scope with: the
// reasons /compact carried forward, and the instruction files the session loaded.
// The files are named for the whole session rather than the run, because a /clear
// starts a new run under the same decision.
func (s *replSession) sessionTaint() []string {
	return append(slices.Clone(s.carriedTaint), s.project.Taint()...)
}
