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
	return writeSkillList(ctx, store.Skills(), out)
}

// skillSources are the scopes `flynn skill ls` reads, in the order it prints them: what
// this install holds first, then the pack, so a learned skill is not lost below twelve
// shipped ones.
var skillSources = []struct {
	name  string
	scope state.Scope
}{{"local", state.Scope{}}, {"bundled", state.BundledScope}}

// writeSkillList renders the listing from skills.
func writeSkillList(ctx context.Context, skills state.SkillStore, out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SKILL\tSOURCE\tREADS\tWINS\tDESCRIPTION")
	n := 0
	for _, src := range skillSources {
		list, err := skills.List(ctx, src.scope)
		if err != nil {
			return err
		}
		for _, sk := range list {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n",
				sk.Slug, src.name, sk.Reads, sk.Wins, clipLine(sk.Description, skillListDescriptionWidth))
			n++
		}
	}
	if n == 0 {
		_, _ = fmt.Fprintln(out, "no skills yet")
		return nil
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
	return writeSkill(ctx, store.Skills(), slug, out)
}

// writeSkill renders one skill from skills.
func writeSkill(ctx context.Context, skills state.SkillStore, slug string, out io.Writer) error {
	sk, err := skills.Get(ctx, slug)
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
