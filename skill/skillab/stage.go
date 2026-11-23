package skillab

import (
	"fmt"
	"io/fs"
	"os"
)

const (
	// ChecksDir holds, beside the exercise set, one directory per fixture of files
	// copied into the working directory after the agent stops and before any check
	// runs: held-out tests the agent never saw. Like [FixturesDir] it sits outside the
	// pack.
	ChecksDir = "checks"

	// HarnessDir is the directory in the working directory where [Set.Stage] leaves
	// what a check needs to judge the run. A scope check must exclude it.
	HarnessDir = ".flynn-ab"

	// ReportFile is the agent's final report, under [HarnessDir]. An honesty check
	// compares it with what the run did.
	ReportFile = "report.txt"

	// TranscriptFile is the turn by turn record of the run, under [HarnessDir].
	TranscriptFile = "transcript.txt"

	// PristineDir is the fixture as the agent first received it, under [HarnessDir],
	// so a check can diff what the run changed.
	PristineDir = "pristine"
)

// Stage prepares workdir for the checks after the agent has stopped. It replaces
// [HarnessDir] with a fresh pristine copy of e's fixture, the final report and the
// transcript, then copies the fixture's [ChecksDir] files over the tree.
//
// The agent has had the directory to itself, so nothing in it is trusted: every write
// goes through an [os.Root] on workdir, which refuses a path that resolves outside it,
// and a symlink the agent left where a harness file belongs is replaced and not
// followed. A staging error means the run left the directory in a state the harness
// cannot place its files in, and the caller decides what that scores.
func (s Set) Stage(e Exercise, workdir, report, transcript string) error {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := root.RemoveAll(HarnessDir); err != nil {
		return fmt.Errorf("skillab: clear %s: %w", HarnessDir, err)
	}
	if err := root.MkdirAll(HarnessDir+"/"+PristineDir, 0o750); err != nil {
		return fmt.Errorf("skillab: make %s: %w", HarnessDir, err)
	}
	for name, body := range map[string]string{ReportFile: report, TranscriptFile: transcript} {
		if err := root.WriteFile(HarnessDir+"/"+name, []byte(body), 0o600); err != nil {
			return fmt.Errorf("skillab: write %s: %w", name, err)
		}
	}
	if e.Fixture == "" {
		return nil
	}
	if _, err := copyTree(s.fsys, path(path(s.dir, FixturesDir), e.Fixture), root, HarnessDir+"/"+PristineDir, "fixture "+e.Fixture, false); err != nil {
		return err
	}
	overlay := path(path(s.dir, ChecksDir), e.Fixture)
	if !s.has(overlay) {
		return nil
	}
	_, err = copyTree(s.fsys, overlay, root, ".", "checks for "+e.Fixture, true)
	return err
}

func (s Set) has(p string) bool {
	if s.fsys == nil {
		return false
	}
	info, err := fs.Stat(s.fsys, p)
	return err == nil && info.IsDir()
}
