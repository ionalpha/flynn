package skillstyle_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/skill/skillstyle"
)

// TestRefusals covers one clean example per rule. Each is a mark with no legitimate
// use in a skill, which is the whole basis for refusing it without an escape.
func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, line, rule string
	}{
		{"em-dash", "Read the diff — then cut it.", "em-dash"},
		{"horizontal bar", "Read the diff ― then cut it.", "em-dash"},
		{"en-dash", "Run it 3–5 times.", "en-dash"},
		{"ellipsis", "Then wait…", "ellipsis character"},
		{"record link", "As decided in @task:acca6500-1234.", "internal record link"},
		{"record id", "See task acca6500 for the reasoning.", "internal record id"},
		{"backticked record id", "Tracked on note `d150e881`.", "internal record id"},
		{"uuid", "Recorded as 65e5d9af-c91a-471f-b397-8343329c8218.", "uuid"},
		{"internal name", "The Ion Alpha convention is to branch first.", "internal name"},
		{"competitor", "Unlike superpowers, this one carries a check.", "competitor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := skillstyle.Check("SKILL.md", []byte(tc.line))
			if len(got) != 1 {
				t.Fatalf("Check(%q) returned %d findings, want 1: %s", tc.line, len(got), skillstyle.Report(got))
			}
			if got[0].Rule != tc.rule {
				t.Errorf("rule = %q, want %q", got[0].Rule, tc.rule)
			}
			if got[0].Why == "" {
				t.Error("a finding with no advice is one an author cannot act on")
			}
		})
	}
}

// TestOrdinaryProseIsLeftAlone is the other half of the bargain. A gate that fires
// on writing a skill is entitled to do gets turned off, so the cases most likely to
// be mistaken for a refusal are pinned as passing.
func TestOrdinaryProseIsLeftAlone(t *testing.T) {
	body := strings.Join([]string{
		"Run the test suite, then read what failed.",
		"A well-formed commit message wraps at 72 columns.",
		"Check out the branch: git checkout -b fix/the-thing.",
		"The commit is e3b0c442, and git show will print it.",
		"Deploy to staging first, production second.",
		"Rebase onto main and force-push with --force-with-lease.",
		"Three dots are fine... and so is a range of 3 to 5.",
	}, "\n")
	if got := skillstyle.Check("SKILL.md", []byte(body)); len(got) != 0 {
		t.Errorf("ordinary prose was refused:\n%s", skillstyle.Report(got))
	}
}

// TestFindingIsAddressed pins the position, because a report that names a file and
// not a line is one an author has to go looking through.
func TestFindingIsAddressed(t *testing.T) {
	got := skillstyle.Check("references/checklist.md", []byte("first line\nsecond — line\n"))
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Line != 2 || got[0].Column != 8 {
		t.Errorf("finding at %d:%d, want 2:8", got[0].Line, got[0].Column)
	}
	if s := got[0].String(); !strings.HasPrefix(s, "references/checklist.md:2:8: em-dash:") {
		t.Errorf("String() = %q, not the file:line:column form", s)
	}
}

// TestEveryFindingIsReported keeps the gate from becoming a one-at-a-time loop. An
// author fixing twenty marks across twenty skills, one CI run each, is the cost this
// avoids.
func TestEveryFindingIsReported(t *testing.T) {
	got := skillstyle.Check("SKILL.md", []byte("a — b — c\nd … e"))
	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3:\n%s", len(got), skillstyle.Report(got))
	}
	if got[0].Column >= got[1].Column {
		t.Error("findings on one line are not ordered by column")
	}
}

// TestCheckFSReadsEverythingInThePack covers the walk: a reference document is read
// by the same person as the body and is held to the same rules, and a directory with
// nothing wrong in it reports nothing.
func TestCheckFSReadsEverythingInThePack(t *testing.T) {
	pack := fstest.MapFS{
		"skills/tidy-diff/SKILL.md":                &fstest.MapFile{Data: []byte("---\nname: tidy-diff\n---\n\nCut the diff.\n")},
		"skills/tidy-diff/references/checklist.md": &fstest.MapFile{Data: []byte("Delete what the change does not need — then read it again.\n")},
		"skills/tidy-diff/scripts/run.sh":          &fstest.MapFile{Data: []byte("#!/bin/sh\nexit 0\n")},
	}

	got, err := skillstyle.CheckFS(pack, "skills")
	if err != nil {
		t.Fatalf("CheckFS: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want the one in the reference document:\n%s", len(got), skillstyle.Report(got))
	}
	if got[0].Path != "skills/tidy-diff/references/checklist.md" {
		t.Errorf("finding is against %q, want the reference document", got[0].Path)
	}

	if _, err := skillstyle.CheckFS(pack, "no-such-root"); err == nil {
		t.Error("CheckFS over a root that is not there returned no error")
	}
}

// TestReportIsEmptyForACleanPack lets a caller test the report rather than the
// count, which is what makes a failure message readable.
func TestReportIsEmptyForACleanPack(t *testing.T) {
	if got := skillstyle.Report(nil); got != "" {
		t.Errorf("Report(nil) = %q, want the empty string", got)
	}
	got := skillstyle.Report(skillstyle.Check("SKILL.md", []byte("a — b\nc … d")))
	if lines := strings.Split(got, "\n"); len(lines) != 2 {
		t.Errorf("Report rendered %d lines for 2 findings:\n%s", len(lines), got)
	}
}

// unreadableFS opens every file with an error, which is what a pack with a file the
// process cannot read looks like from inside the walk.
type unreadableFS struct{ fs.FS }

func (u unreadableFS) Open(name string) (fs.File, error) {
	f, err := u.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if st, serr := f.Stat(); serr == nil && st.IsDir() {
		return f, nil
	}
	_ = f.Close()
	return nil, errors.New("permission denied")
}

// TestUnreadableFileIsAFindingNotAnError keeps one broken file from hiding the
// findings in the files beside it. A gate that stops at the first unreadable path
// reports a clean pack for the wrong reason.
func TestUnreadableFileIsAFindingNotAnError(t *testing.T) {
	pack := fstest.MapFS{"skills/tidy-diff/SKILL.md": &fstest.MapFile{Data: []byte("Cut the diff.\n")}}

	got, err := skillstyle.CheckFS(unreadableFS{pack}, "skills")
	if err != nil {
		t.Fatalf("CheckFS: %v", err)
	}
	if len(got) != 1 || got[0].Rule != "unreadable" {
		t.Fatalf("got %d findings, want one unreadable:\n%s", len(got), skillstyle.Report(got))
	}
	if !strings.Contains(got[0].Why, "permission denied") {
		t.Errorf("the finding does not say why the file could not be read: %s", got[0])
	}
}
