package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/notices"
)

// startupNotices prints any notice that applies to this build, and starts the check for a
// new feed in the background.
//
// It writes to stderr, never stdout. A user who pipes `flynn describe` into a file is
// entitled to get their output and not our announcements, and a channel that could corrupt
// a pipeline is a channel people would be right to turn off.
//
// Nothing here can fail the command. A missing feed, an unreadable cache, a cached
// document that no longer verifies, and a network that is not there are all the same
// thing: no notices, and the user's work proceeds exactly as if this code did not exist.
func startupNotices(ctx context.Context, dataDir string) {
	c := notices.NewClient(dataDir, version.String())
	c.Show(os.Stderr)
	applyRuntimeFloors(c)
	// The fetch is off the hot path on purpose: what it finds is shown by the next run.
	// The user did not ask us to talk to a server, they asked us to do a job, and the job
	// must not wait on our network.
	c.Background(ctx)
}

// applyRuntimeFloors raises the local inference runtimes' minimum-version gates to whatever
// the signed feed says, if that is higher than what this binary was built with. It is how a
// model-parser vulnerability disclosed after a release reaches an installation that has not
// upgraded, which is the one case where the compiled-in floor is exactly the floor that
// does not know about the bug.
//
// Raise takes the maximum and there is no operation that lowers a floor, so this can only
// ever refuse more. A feed that is hostile, forged (it would not verify), or simply wrong
// cannot use this path to let a vulnerable runtime through: the worst it achieves is that
// Flynn declines to run a local model and says why.
func applyRuntimeFloors(c *notices.Client) {
	for _, f := range c.Floors() {
		inference.Raise(f.Runtime, inference.ParseVersion(f.MinVersion), f.AdvisoryID)
	}
}

// runNotices implements `flynn notices`: show every notice that applies to this build,
// with the detail that the one-line startup summary leaves out.
func runNotices(args []string, dataDir string) error {
	c := notices.NewClient(dataDir, version.String())

	refresh := false
	all := false
	for _, a := range args {
		switch a {
		case "--refresh":
			refresh = true
		case "--all":
			all = true
		default:
			return errors.New("usage: flynn notices [--refresh] [--all]")
		}
	}

	if !notices.Enabled() {
		_, _ = fmt.Fprintf(os.Stdout, "the notice channel is off (%s is set, or this build carries no publisher key)\n", notices.OffEnv)
		return nil
	}

	if refresh {
		// Here, and only here, a failed check is worth reporting: the user asked for it.
		// Everywhere else it is silent, because nobody asked.
		if err := c.Refresh(context.Background()); err != nil {
			return fmt.Errorf("checking for notices: %w", err)
		}
	}

	feed, trust, ok := notices.Cached(c.Store, c.Ring)
	if !ok {
		_, _ = fmt.Fprintln(os.Stdout, "no notices have been received yet; run `flynn notices --refresh` to check now")
		return nil
	}

	list := feed.Notices
	if !all {
		list = applicable(feed, version.String())
	}
	if len(list) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no notices apply to this version")
	}
	for _, n := range list {
		_, _ = fmt.Fprintf(os.Stdout, "\n[%s] %s\n  %s\n", n.Severity, n.ID, n.Summary)
		if n.Detail != "" {
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", n.Detail)
		}
		if n.URL != "" {
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", n.URL)
		}
		if n.FixedIn != "" {
			_, _ = fmt.Fprintf(os.Stdout, "  fixed in %s\n", n.FixedIn)
		}
	}

	if feed.Stale(c.Clock.Now()) {
		_, _ = fmt.Fprintln(os.Stdout, "\nwarning: this feed has not been refreshed recently, so it may not be the current list")
	}
	_, _ = fmt.Fprintf(os.Stdout, "\nfeed version %d, issued %s\n", feed.Version, feed.Issued.Format("2006-01-02"))
	_ = trust
	return nil
}

// applicable filters a feed down to the notices that are about the running version,
// ignoring whether they have already been shown: `flynn notices` is the command a user
// runs to read them again on purpose.
func applicable(f notices.Feed, v string) []notices.Notice {
	var out []notices.Notice
	for _, n := range f.Notices {
		if notices.Applies(n, v) {
			out = append(out, n)
		}
	}
	return out
}
