package main

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/state"
)

// TestSkillLsListsTheShippedPackOnAFreshInstall: a user who has never run anything
// can see what the binary knows, because opening the store seeds the pack.
func TestSkillLsListsTheShippedPackOnAFreshInstall(t *testing.T) {
	got := runCLIIn(t, t.TempDir(), "skill", "ls")
	if got.code != 0 {
		t.Fatalf("exit = %d, stderr %q", got.code, got.stderr)
	}
	pack, err := bundled.Skills()
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	if len(pack) == 0 {
		t.Fatal("the binary ships no skills, so this test proves nothing")
	}
	for _, sk := range pack {
		if !strings.Contains(got.stdout, sk.Slug) {
			t.Errorf("skill ls does not list bundled skill %q:\n%s", sk.Slug, got.stdout)
		}
	}
	if !strings.Contains(got.stdout, "bundled") {
		t.Errorf("skill ls does not say where the pack came from:\n%s", got.stdout)
	}
}

// TestSkillLsAndShowALearnedSkill: a skill this install holds is listed beside the
// pack, marked local, with the reads and wins that rank it, and show prints it whole.
func TestSkillLsAndShowALearnedSkill(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store, err := openDataStore(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Skills().Upsert(ctx, state.Skill{
		Slug:        "retry-flaky-fetch",
		Name:        "retry-flaky-fetch",
		Description: "Retry a fetch that fails intermittently.\nBack off between tries.",
		Body:        "1. Retry three times.\n2. Back off.\n",
		Check:       "go test ./fetch",
		Reads:       4,
		Wins:        3,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ls := runCLIIn(t, dir, "skill", "ls")
	if ls.code != 0 {
		t.Fatalf("ls exit = %d, stderr %q", ls.code, ls.stderr)
	}
	var row string
	for _, line := range strings.Split(ls.stdout, "\n") {
		if strings.HasPrefix(line, "retry-flaky-fetch") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("skill ls does not list the learned skill:\n%s", ls.stdout)
	}
	for _, want := range []string{"local", " 4 ", " 3 ", "Retry a fetch that fails intermittently. Back off"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q is missing %q", row, want)
		}
	}

	show := runCLIIn(t, dir, "skill", "show", "retry-flaky-fetch")
	if show.code != 0 {
		t.Fatalf("show exit = %d, stderr %q", show.code, show.stderr)
	}
	for _, want := range []string{"retry-flaky-fetch (local, version 1)", "read 4", "check: go test ./fetch", "1. Retry three times."} {
		if !strings.Contains(show.stdout, want) {
			t.Errorf("skill show is missing %q:\n%s", want, show.stdout)
		}
	}
}

// TestSkillShowABundledSkill: show reaches into the pack as well, and names it bundled.
func TestSkillShowABundledSkill(t *testing.T) {
	pack, err := bundled.Skills()
	if err != nil || len(pack) == 0 {
		t.Fatalf("read pack: %v (%d skills)", err, len(pack))
	}
	got := runCLIIn(t, t.TempDir(), "skill", "show", pack[0].Slug)
	if got.code != 0 {
		t.Fatalf("exit = %d, stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, pack[0].Slug+" (bundled") {
		t.Errorf("skill show does not name %s as bundled:\n%s", pack[0].Slug, got.stdout)
	}
}

// TestSkillCommandRefusals pins the exit codes: a malformed command line is a usage
// error (2) and a skill that does not exist is a command error (1) that says how to
// find the real ones.
func TestSkillCommandRefusals(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no subcommand", []string{"skill"}, 2, "usage: flynn skill"},
		{"unknown subcommand", []string{"skill", "frobnicate"}, 2, "usage: flynn skill"},
		{"ls with an argument", []string{"skill", "ls", "extra"}, 2, "usage: flynn skill"},
		{"show without a skill", []string{"skill", "show"}, 2, "usage: flynn skill"},
		{"show an unknown skill", []string{"skill", "show", "no-such-skill"}, 1, "flynn skill ls"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runCLIIn(t, t.TempDir(), c.args...)
			if got.code != c.wantCode {
				t.Fatalf("exit = %d, want %d (stderr %q)", got.code, c.wantCode, got.stderr)
			}
			if !strings.Contains(got.stderr, c.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", got.stderr, c.wantErr)
			}
		})
	}
}

// TestClipLine: a description is flattened to one line and never wider than the
// column, and a cut is marked so it does not read as the whole text.
func TestClipLine(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"two\nlines  here", 20, "two lines here"},
		{"exactly ten", 11, "exactly ten"},
		{"one more than fits", 10, "one mor..."},
		{"héllo wörld again", 10, "héllo w..."},
	}
	for _, c := range cases {
		got := clipLine(c.in, c.width)
		if got != c.want {
			t.Errorf("clipLine(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
		if n := utf8.RuneCountInString(got); n > c.width {
			t.Errorf("clipLine(%q, %d) is %d runes wide", c.in, c.width, n)
		}
	}
}
