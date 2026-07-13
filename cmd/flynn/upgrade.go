package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/ionalpha/flynn/internal/selfupdate"
	"github.com/ionalpha/flynn/internal/version"
)

// updater is the self-update surface the version and upgrade commands drive: enumerate
// the releases that exist, decide what installing one would mean, and install it. It is
// an interface so the commands around it (what they print, what they refuse, what they
// ask before doing) are exercised without reaching the network.
type updater interface {
	List(ctx context.Context) ([]selfupdate.Release, error)
	RecordSeen(releases []selfupdate.Release)
	Check(ctx context.Context, req selfupdate.Request) (selfupdate.Plan, error)
	Apply(ctx context.Context, plan selfupdate.Plan) error
}

// newUpdater builds the updater a command runs against. It is a variable so a test can
// stand in one whose answers it controls; the binary always gets the real one.
var newUpdater = func(dataDir string) updater { return selfupdate.New(dataDir) }

// runVersion prints what this binary is, and can list what else exists.
//
//	flynn version           what am I running
//	flynn version list      what releases exist, and which one am I on
//	flynn version check     is there a newer one (exit 1 if yes, for scripts)
func runVersion(args []string, dataDir string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pre := fs.Bool("pre", false, "include prereleases")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sub := ""
	if fs.NArg() > 0 {
		sub = fs.Arg(0)
	}
	switch sub {
	case "":
		return printBuild(os.Stdout)
	case "list":
		return listVersions(os.Stdout, newUpdater(dataDir), *pre)
	case "check":
		return checkVersion(os.Stdout, os.Stderr, newUpdater(dataDir), *pre)
	default:
		return errors.New("usage: flynn version [list|check] [--pre]")
	}
}

// printBuild reports the running build, including where it lives, because "which flynn
// am I actually running" is the question behind most version confusion.
func printBuild(out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		exe = "unknown"
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "version:\t%s\n", version.String())
	_, _ = fmt.Fprintf(tw, "platform:\t%s/%s\n", runtime.GOOS, runtime.GOARCH)
	_, _ = fmt.Fprintf(tw, "go:\t%s\n", runtime.Version())
	_, _ = fmt.Fprintf(tw, "path:\t%s\n", exe)
	return tw.Flush()
}

func listVersions(out io.Writer, u updater, pre bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	releases, err := u.List(ctx)
	if err != nil {
		return err
	}
	u.RecordSeen(releases)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VERSION\tPUBLISHED\t")
	for _, r := range releases {
		if r.Prerelease && !pre && !r.Current {
			continue
		}
		mark := ""
		switch {
		case r.Current:
			mark = "  <- running"
		case r.Prerelease:
			mark = "  (prerelease)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Version, r.PublishedAt.Format("2006-01-02"), mark)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// The listing is not signed and is never treated as if it were. Saying so here keeps
	// the guarantee legible: what makes an upgrade safe is the check that happens when
	// one is installed, not this table.
	_, _ = fmt.Fprintln(out, "\nThis listing is unverified. Every version's signature is checked before it is installed.")
	return nil
}

// checkVersion reports whether an upgrade is available, and exits non-zero when one
// is, so a script or a monitor can act on it without parsing text.
func checkVersion(out, errOut io.Writer, u updater, pre bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	plan, err := u.Check(ctx, selfupdate.Request{AllowPrerelease: pre})
	if err != nil {
		return err
	}
	if plan.Warning != "" {
		_, _ = fmt.Fprintln(errOut, "warning:", plan.Warning)
	}
	if plan.UpToDate() {
		_, _ = fmt.Fprintf(out, "flynn %s is the latest release.\n", plan.Current)
		return nil
	}
	_, _ = fmt.Fprintf(out, "flynn %s is available (running %s). Run `flynn upgrade` to install it.\n", plan.Target, plan.Current)
	return errUpgradeAvailable
}

// errUpgradeAvailable makes `flynn version check` exit non-zero when an upgrade is
// waiting, so a script or a monitor can branch on the exit code. It carries no
// message, because nothing went wrong: the dispatch prints nothing for an error whose
// message is empty.
var errUpgradeAvailable error = silent{}

type silent struct{}

func (silent) Error() string { return "" }

// confirmFunc asks the operator to approve an irreversible action. confirm is the real
// one, which needs a terminal; a test supplies its own to drive both answers.
type confirmFunc func(question string) (bool, error)

// confirm asks the operator to approve an irreversible action. With no terminal to ask
// on, it does not guess: an upgrade that runs unattended has to say --yes, so a script
// that pipes something unexpected into flynn cannot accidentally replace the binary.
func confirm(question string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, errors.New("no terminal to confirm on: re-run with --yes to upgrade unattended")
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s [y/N] ", question)
	// A blank line, a closed stdin, or anything else unreadable leaves answer empty,
	// which is a "no". The only way through is an explicit yes.
	var answer string
	_, _ = fmt.Scanln(&answer)
	return answer == "y" || answer == "Y" || answer == "yes", nil
}

// runUpgrade replaces the running binary with a newer, verified release.
func runUpgrade(args []string, dataDir string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	to := fs.String("to", "", "install this exact version instead of the newest")
	pre := fs.Bool("pre", false, "consider prereleases")
	check := fs.Bool("check", false, "report what would be installed and stop")
	allowDowngrade := fs.Bool("allow-downgrade", false, "permit installing an older version than the one running")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	return upgradeTo(ctx, os.Stdout, os.Stderr, newUpdater(dataDir), selfupdate.Request{
		To:              *to,
		AllowPrerelease: *pre,
		AllowDowngrade:  *allowDowngrade,
	}, *check, *yes, confirm)
}

// upgradeTo runs the upgrade the parsed flags asked for: verify what installing the
// requested release would mean, show the operator the facts that verified, and install
// it only once it is either confirmed or waived with --yes. checkOnly stops after the
// report, which is what `flynn upgrade --check` is.
func upgradeTo(ctx context.Context, out, errOut io.Writer, u updater, req selfupdate.Request, checkOnly, yes bool, ask confirmFunc) error {
	plan, err := u.Check(ctx, req)
	if err != nil {
		return err
	}

	if plan.Warning != "" {
		_, _ = fmt.Fprintln(errOut, "warning:", plan.Warning)
	}
	if plan.UpToDate() && !req.AllowDowngrade {
		_, _ = fmt.Fprintf(out, "flynn %s is already the latest release.\n", plan.Current)
		return nil
	}

	// Everything printed here is a verified fact, not a claim from the server: the
	// signer, the commit, and the transparency-log entry all came out of a signature
	// that checked out against the trust root in this binary. The operator can go and
	// look the log entry up.
	_, _ = fmt.Fprintf(out, "%s -> %s\n", plan.Current, plan.Target)
	if plan.Downgrade {
		_, _ = fmt.Fprintf(out, "  DOWNGRADE: this is older than what is installed\n")
	}
	_, _ = fmt.Fprintf(out, "  signed by  %s\n", plan.Provenance.SignerIdentity)
	_, _ = fmt.Fprintf(out, "  built from %s\n", plan.Provenance.Commit)
	_, _ = fmt.Fprintf(out, "  logged at  rekor.sigstore.dev index %d (%s)\n",
		plan.Provenance.LogIndex, plan.Provenance.LoggedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(out, "  artifact   %s\n", plan.Asset)
	_, _ = fmt.Fprintf(out, "  sha256     %s\n", plan.Digest)
	_, _ = fmt.Fprintf(out, "  installs   %s\n", plan.Path)

	if checkOnly {
		return nil
	}
	if !yes {
		ok, err := ask(fmt.Sprintf("Install %s?", plan.Target))
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(out, "cancelled")
			return nil
		}
	}

	_, _ = fmt.Fprintln(out, "\ndownloading and verifying...")
	if err := u.Apply(ctx, plan); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "flynn %s installed.\n", plan.Target)
	return nil
}
