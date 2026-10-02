package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/state"
)

// errSkillUsage is the usage line for `flynn skill`.
var errSkillUsage = errors.New("usage: flynn skill ls | flynn skill show <skill> | flynn skill ab <skill> [--repeats n] [--exercises dir]")

// skillListDescriptionWidth caps the description column of `flynn skill ls`, so one
// long description does not push every row past the width of a terminal. The whole
// text is one `flynn skill show` away.
const skillListDescriptionWidth = 72

// dispatchSkill routes `flynn skill <sub>`.
func dispatchSkill(args []string, modelSpec, dataDir string, out io.Writer) error {
	if len(args) == 0 {
		return errSkillUsage
	}
	switch args[0] {
	case "ls", "list":
		if len(args) != 1 {
			return errSkillUsage
		}
		return listSkills(dataDir, out)
	case "show":
		if len(args) != 2 {
			return errSkillUsage
		}
		return showSkill(dataDir, args[1], out)
	case "ab":
		return runSkillAB(args[1:], modelSpec, dataDir, out)
	default:
		return errSkillUsage
	}
}

// listSkills prints every skill this install can offer a run: the pack shipped in the
// binary and the skills this install learned. Neither needs a model, so a machine with
// no provider configured can still see what its agent knows.
func listSkills(dataDir string, out io.Writer) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	local, err := store.Skills().List(ctx, state.Scope{})
	if err != nil {
		return err
	}
	bundled, err := store.Skills().List(ctx, state.BundledScope)
	if err != nil {
		return err
	}
	if len(local)+len(bundled) == 0 {
		_, _ = fmt.Fprintln(out, "no skills yet")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SKILL\tSOURCE\tREADS\tWINS\tDESCRIPTION")
	for _, group := range []struct {
		source string
		skills []state.Skill
	}{{"local", local}, {"bundled", bundled}} {
		for _, sk := range group.skills {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n",
				sk.Slug, group.source, sk.Reads, sk.Wins, clipLine(sk.Description, skillListDescriptionWidth))
		}
	}
	return tw.Flush()
}

// showSkill prints one skill whole: what it is for, how runs have taken it up, the
// check that re-grades it, and its body, which is the text a run reads.
func showSkill(dataDir, slug string, out io.Writer) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	sk, err := store.Skills().Get(ctx, slug)
	if errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("no skill %q; `flynn skill ls` lists them", slug)
	}
	if err != nil {
		return err
	}

	source := "local"
	if sk.Scope == state.BundledScope {
		source = "bundled"
	}
	_, _ = fmt.Fprintf(out, "%s (%s, version %d)\n", sk.Slug, source, sk.Version)
	if sk.Description != "" {
		_, _ = fmt.Fprintf(out, "\n%s\n", sk.Description)
	}
	_, _ = fmt.Fprintf(out, "\noffered %d, read %d, read on a successful run %d\n", sk.Offers, sk.Reads, sk.Wins)
	if sk.Check != "" {
		_, _ = fmt.Fprintf(out, "check: %s\n", sk.Check)
	}
	_, _ = fmt.Fprintf(out, "\n%s\n", strings.TrimRight(sk.Body, "\n"))
	return nil
}

// clipLine flattens s onto one line and cuts it to at most width runes, marking a cut
// with three dots so a clipped description does not read as the whole of it.
func clipLine(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-3]) + "..."
}
