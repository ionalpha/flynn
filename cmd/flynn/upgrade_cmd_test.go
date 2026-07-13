package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/release"
	"github.com/ionalpha/flynn/internal/selfupdate"
)

// upgradeEpoch is the fixed instant every synthetic release in this file is published
// and logged at. A test never asks the wall clock what time it is: a printed timestamp
// is only assertable when the test chose it.
var upgradeEpoch = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// stubUpdater stands in for the real self-update path. Every answer is stated by the
// test, so the command's own behaviour (what it prints, what it refuses, what it asks
// before doing, and whether it installs) is exercised with no network and no binary
// swapped. It also records what it was asked, so a test can prove the command did not
// install when it should not have.
type stubUpdater struct {
	releases []selfupdate.Release
	listErr  error

	plan     selfupdate.Plan
	checkErr error
	applyErr error

	seen    int                // RecordSeen calls
	applied int                // Apply calls
	req     selfupdate.Request // the last request Check was asked for
	sawPlan selfupdate.Plan    // the plan Apply was handed
}

func (s *stubUpdater) List(context.Context) ([]selfupdate.Release, error) {
	return s.releases, s.listErr
}

func (s *stubUpdater) RecordSeen([]selfupdate.Release) { s.seen++ }

func (s *stubUpdater) Check(_ context.Context, req selfupdate.Request) (selfupdate.Plan, error) {
	s.req = req
	return s.plan, s.checkErr
}

func (s *stubUpdater) Apply(_ context.Context, p selfupdate.Plan) error {
	s.applied++
	s.sawPlan = p
	return s.applyErr
}

// mustVersion parses a release version or fails the test; selfupdate.Version is opaque
// by design, so this is the only way to state one.
func mustVersion(t *testing.T, s string) selfupdate.Version {
	t.Helper()
	v, ok := selfupdate.ParseVersion(s)
	if !ok {
		t.Fatalf("%q is not a version", s)
	}
	return v
}

// upgradePlan builds a verified-looking plan from current to target, carrying the
// provenance facts the command prints as the reason an operator can trust the install.
func upgradePlan(t *testing.T, current, target string) selfupdate.Plan {
	t.Helper()
	return selfupdate.Plan{
		Current: mustVersion(t, current),
		Target:  mustVersion(t, target),
		Provenance: release.Provenance{
			Tag:            target,
			SignerIdentity: "https://github.com/ionalpha/flynn/.github/workflows/release.yml@refs/tags/" + target,
			Commit:         "0123456789abcdef0123456789abcdef01234567",
			LogIndex:       987654,
			LoggedAt:       upgradeEpoch,
		},
		Asset:  "flynn_linux_amd64.tar.gz",
		Digest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		URL:    "https://example.invalid/flynn_linux_amd64.tar.gz",
		Path:   "/usr/local/bin/flynn",
	}
}

// withUpdater points the version and upgrade commands at u for the length of the test,
// and restores the real factory afterwards.
func withUpdater(t *testing.T, u updater) {
	t.Helper()
	prev := newUpdater
	newUpdater = func(string) updater { return u }
	t.Cleanup(func() { newUpdater = prev })
}

// alwaysConfirm and neverConfirm are the two answers an operator can give.
func alwaysConfirm(string) (bool, error) { return true, nil }
func neverConfirm(string) (bool, error)  { return false, nil }

// TestVersionListShowsWhatExistsAndSaysItIsUnverified: the listing marks the running
// release, hides prereleases unless asked, and states plainly that nothing in the table
// is verified, because the guarantee lives in the install, not in this table.
func TestVersionListShowsWhatExistsAndSaysItIsUnverified(t *testing.T) {
	u := &stubUpdater{releases: []selfupdate.Release{
		{Version: mustVersion(t, "v1.2.0"), PublishedAt: upgradeEpoch},
		{Version: mustVersion(t, "v1.3.0-rc.1"), Prerelease: true, PublishedAt: upgradeEpoch},
		{Version: mustVersion(t, "v1.1.0"), PublishedAt: upgradeEpoch, Current: true},
	}}

	var out bytes.Buffer
	if err := listVersions(&out, u, false); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := out.String()
	for _, want := range []string{"VERSION", "v1.2.0", "v1.1.0", "<- running", "2026-03-04", "unverified"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "v1.3.0-rc.1") {
		t.Fatalf("a prerelease was listed without --pre:\n%s", got)
	}
	if u.seen != 1 {
		t.Fatalf("the newest offered release was recorded %d times, want once", u.seen)
	}

	// --pre shows the prerelease, labelled as one.
	var pre bytes.Buffer
	if err := listVersions(&pre, u, true); err != nil {
		t.Fatalf("list --pre: %v", err)
	}
	if !strings.Contains(pre.String(), "v1.3.0-rc.1") || !strings.Contains(pre.String(), "(prerelease)") {
		t.Fatalf("--pre listing missing the labelled prerelease:\n%s", pre.String())
	}
}

// TestVersionListShowsTheRunningPrereleaseWithoutPre: a machine running a prerelease
// must still see the version it is on, or the table would omit the one row that matters
// most to it.
func TestVersionListShowsTheRunningPrereleaseWithoutPre(t *testing.T) {
	u := &stubUpdater{releases: []selfupdate.Release{
		{Version: mustVersion(t, "v1.3.0-rc.1"), Prerelease: true, PublishedAt: upgradeEpoch, Current: true},
	}}
	var out bytes.Buffer
	if err := listVersions(&out, u, false); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "v1.3.0-rc.1") || !strings.Contains(out.String(), "<- running") {
		t.Fatalf("the running prerelease was hidden from its own listing:\n%s", out.String())
	}
}

// TestVersionListSurfacesAListingFailure: a listing that cannot be fetched is an error,
// never an empty table that reads as "there are no releases".
func TestVersionListSurfacesAListingFailure(t *testing.T) {
	u := &stubUpdater{listErr: errors.New("HTTP 503")}
	var out bytes.Buffer
	if err := listVersions(&out, u, false); err == nil {
		t.Fatal("a failed listing must be reported")
	}
	if u.seen != 0 {
		t.Fatal("a failed listing must not be recorded as seen")
	}
	if out.Len() != 0 {
		t.Fatalf("a failed listing printed a table: %q", out.String())
	}
}

// TestVersionCheckExitsNonZeroOnlyWhenAnUpgradeWaits: the exit code is the contract a
// monitor branches on, and the sentinel carries no message because nothing went wrong.
func TestVersionCheckExitsNonZeroOnlyWhenAnUpgradeWaits(t *testing.T) {
	t.Run("up to date", func(t *testing.T) {
		u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.2.0")}
		var out, errOut bytes.Buffer
		if err := checkVersion(&out, &errOut, u, false); err != nil {
			t.Fatalf("up-to-date check = %v, want nil", err)
		}
		if !strings.Contains(out.String(), "is the latest release") {
			t.Fatalf("check output = %q", out.String())
		}
	})

	t.Run("upgrade available", func(t *testing.T) {
		u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0")}
		var out, errOut bytes.Buffer
		err := checkVersion(&out, &errOut, u, true)
		if !errors.Is(err, errUpgradeAvailable) {
			t.Fatalf("check = %v, want errUpgradeAvailable", err)
		}
		if err.Error() != "" {
			t.Fatalf("the sentinel must carry no message, got %q", err.Error())
		}
		if !strings.Contains(out.String(), "v1.3.0 is available") {
			t.Fatalf("check output = %q", out.String())
		}
		if !u.req.AllowPrerelease {
			t.Fatal("--pre was not carried into the request")
		}
	})

	t.Run("warning goes to stderr and does not fail the check", func(t *testing.T) {
		plan := upgradePlan(t, "v1.2.0", "v1.2.0")
		plan.Warning = "the release listing no longer offers v1.4.0"
		u := &stubUpdater{plan: plan}
		var out, errOut bytes.Buffer
		if err := checkVersion(&out, &errOut, u, false); err != nil {
			t.Fatalf("check: %v", err)
		}
		if !strings.Contains(errOut.String(), "warning:") || !strings.Contains(errOut.String(), "no longer offers v1.4.0") {
			t.Fatalf("the listing warning did not reach stderr: %q", errOut.String())
		}
		if strings.Contains(out.String(), "warning:") {
			t.Fatalf("the warning polluted the command's own output: %q", out.String())
		}
	})

	t.Run("a failed check is an error", func(t *testing.T) {
		u := &stubUpdater{checkErr: errors.New("the release listing offered no releases")}
		var out, errOut bytes.Buffer
		if err := checkVersion(&out, &errOut, u, false); err == nil || errors.Is(err, errUpgradeAvailable) {
			t.Fatalf("check = %v, want a real failure", err)
		}
	})
}

// TestUpgradeInstallsOnlyWhatItShowed: the happy path prints the facts that verified
// (signer, commit, transparency-log entry, digest, destination) and then installs
// exactly the plan it printed.
func TestUpgradeInstallsOnlyWhatItShowed(t *testing.T) {
	u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0")}
	var out, errOut bytes.Buffer
	err := upgradeTo(context.Background(), &out, &errOut, u,
		selfupdate.Request{}, false, true, neverConfirm)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"v1.2.0 -> v1.3.0",
		"signed by",
		"release.yml@refs/tags/v1.3.0",
		"built from 0123456789abcdef",
		"rekor.sigstore.dev index 987654",
		"2026-03-04T05:06:07Z",
		"flynn_linux_amd64.tar.gz",
		"e3b0c44298fc1c14",
		"/usr/local/bin/flynn",
		"flynn v1.3.0 installed.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("upgrade output missing %q:\n%s", want, got)
		}
	}
	if u.applied != 1 {
		t.Fatalf("Apply called %d times, want once", u.applied)
	}
	if u.sawPlan.Target.String() != "v1.3.0" || u.sawPlan.Digest != u.plan.Digest {
		t.Fatalf("the installed plan is not the one shown: %+v", u.sawPlan)
	}
	// --yes waives the question; it must not be asked anyway (neverConfirm would have
	// cancelled the install).
	if strings.Contains(got, "cancelled") {
		t.Fatal("--yes still asked for confirmation")
	}
}

// TestUpgradeCheckStopsAfterTheReport: --check is a report, never an install.
func TestUpgradeCheckStopsAfterTheReport(t *testing.T) {
	u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0")}
	var out, errOut bytes.Buffer
	if err := upgradeTo(context.Background(), &out, &errOut, u,
		selfupdate.Request{}, true, true, alwaysConfirm); err != nil {
		t.Fatalf("upgrade --check: %v", err)
	}
	if u.applied != 0 {
		t.Fatal("--check installed something")
	}
	if !strings.Contains(out.String(), "v1.2.0 -> v1.3.0") {
		t.Fatalf("--check printed no plan:\n%s", out.String())
	}
	if strings.Contains(out.String(), "installed.") {
		t.Fatalf("--check claimed an install:\n%s", out.String())
	}
}

// TestUpgradeNeedsAnAnswerBeforeReplacingTheBinary: without --yes the binary is
// replaced only on an explicit yes, and an unanswerable prompt (no terminal) refuses
// rather than guessing.
func TestUpgradeNeedsAnAnswerBeforeReplacingTheBinary(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0")}
		var out, errOut bytes.Buffer
		if err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{}, false, false, neverConfirm); err != nil {
			t.Fatalf("a declined upgrade is not a failure: %v", err)
		}
		if u.applied != 0 {
			t.Fatal("a declined upgrade installed the binary anyway")
		}
		if !strings.Contains(out.String(), "cancelled") {
			t.Fatalf("a declined upgrade said nothing:\n%s", out.String())
		}
	})

	t.Run("accepted", func(t *testing.T) {
		u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0")}
		var out, errOut bytes.Buffer
		if err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{}, false, false, alwaysConfirm); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if u.applied != 1 {
			t.Fatal("a confirmed upgrade did not install")
		}
	})

	t.Run("unanswerable", func(t *testing.T) {
		u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0")}
		var out, errOut bytes.Buffer
		refuse := func(string) (bool, error) { return false, errors.New("no terminal to confirm on") }
		if err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{}, false, false, refuse); err == nil {
			t.Fatal("an unanswerable prompt must refuse, not proceed")
		}
		if u.applied != 0 {
			t.Fatal("an unanswerable prompt installed the binary")
		}
	})
}

// TestConfirmRefusesWithoutATerminal pins the real prompt's unattended behaviour: the
// test process has no terminal on stdin, so an upgrade that did not say --yes is
// refused rather than answered by whatever happens to be piped in.
func TestConfirmRefusesWithoutATerminal(t *testing.T) {
	ok, err := confirm("Install v1.3.0?")
	if err == nil {
		t.Fatal("confirm must refuse when there is no terminal to ask on")
	}
	if ok {
		t.Fatal("confirm answered yes with nothing to ask")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("the refusal must name the way through, got %q", err)
	}
}

// TestUpgradeRefusalsInstallNothing: a plan that does not verify, and a download or
// install that fails, both end with the running binary untouched and the failure
// reported. The command never claims an install it did not make.
func TestUpgradeRefusalsInstallNothing(t *testing.T) {
	t.Run("the plan does not verify", func(t *testing.T) {
		u := &stubUpdater{checkErr: errors.New("the signed provenance served for v1.3.0 is the provenance of v0.1.0")}
		var out, errOut bytes.Buffer
		err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{}, false, true, alwaysConfirm)
		if err == nil {
			t.Fatal("an unverifiable plan must be refused")
		}
		if u.applied != 0 {
			t.Fatal("an unverifiable plan was installed")
		}
		if out.Len() != 0 {
			t.Fatalf("a refused upgrade printed a plan: %q", out.String())
		}
	})

	t.Run("the download or install fails", func(t *testing.T) {
		u := &stubUpdater{
			plan:     upgradePlan(t, "v1.2.0", "v1.3.0"),
			applyErr: errors.New("the archive does not contain flynn"),
		}
		var out, errOut bytes.Buffer
		err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{}, false, true, alwaysConfirm)
		if err == nil || !strings.Contains(err.Error(), "archive") {
			t.Fatalf("a corrupt archive must be reported, got %v", err)
		}
		if strings.Contains(out.String(), "installed.") {
			t.Fatalf("a failed install claimed success:\n%s", out.String())
		}
	})
}

// TestUpgradeUpToDateAndDowngrade: with nothing newer the command stops and says so,
// unless a downgrade was asked for in as many words, in which case the older target is
// shown with the downgrade called out.
func TestUpgradeUpToDateAndDowngrade(t *testing.T) {
	t.Run("nothing to do", func(t *testing.T) {
		u := &stubUpdater{plan: upgradePlan(t, "v1.3.0", "v1.3.0")}
		var out, errOut bytes.Buffer
		if err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{}, false, true, alwaysConfirm); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if u.applied != 0 {
			t.Fatal("an up-to-date binary was reinstalled")
		}
		if !strings.Contains(out.String(), "already the latest release") {
			t.Fatalf("output = %q", out.String())
		}
	})

	t.Run("an asked-for downgrade is called out", func(t *testing.T) {
		plan := upgradePlan(t, "v1.3.0", "v1.2.0")
		plan.Downgrade = true
		u := &stubUpdater{plan: plan}
		var out, errOut bytes.Buffer
		if err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{To: "v1.2.0", AllowDowngrade: true}, false, true, alwaysConfirm); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if !strings.Contains(out.String(), "DOWNGRADE") {
			t.Fatalf("a downgrade was installed without saying so:\n%s", out.String())
		}
		if u.applied != 1 {
			t.Fatal("the asked-for downgrade was not installed")
		}
		if u.req.To != "v1.2.0" || !u.req.AllowDowngrade {
			t.Fatalf("the request did not carry the operator's flags: %+v", u.req)
		}
	})

	t.Run("--allow-downgrade reinstalls the running version", func(t *testing.T) {
		// Same version both sides: UpToDate, but the operator said allow-downgrade, so the
		// command proceeds rather than short-circuiting.
		u := &stubUpdater{plan: upgradePlan(t, "v1.3.0", "v1.3.0")}
		var out, errOut bytes.Buffer
		if err := upgradeTo(context.Background(), &out, &errOut, u,
			selfupdate.Request{AllowDowngrade: true}, false, true, alwaysConfirm); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if u.applied != 1 {
			t.Fatal("--allow-downgrade did not reinstall")
		}
	})
}

// TestRunVersionDispatch drives the command's own argument handling: the bare form
// reports the running build, the subcommands route to the listing and the check, and an
// unknown one is a usage error rather than a silent no-op.
func TestRunVersionDispatch(t *testing.T) {
	withUpdater(t, &stubUpdater{
		releases: []selfupdate.Release{{Version: mustVersion(t, "v1.2.0"), PublishedAt: upgradeEpoch}},
		plan:     upgradePlan(t, "v1.2.0", "v1.2.0"),
	})
	dir := t.TempDir()

	if err := runVersion(nil, dir); err != nil {
		t.Fatalf("flynn version: %v", err)
	}
	if err := runVersion([]string{"list"}, dir); err != nil {
		t.Fatalf("flynn version list: %v", err)
	}
	if err := runVersion([]string{"check"}, dir); err != nil {
		t.Fatalf("flynn version check (up to date): %v", err)
	}
	if err := runVersion([]string{"wat"}, dir); err == nil {
		t.Fatal("an unknown subcommand must be a usage error")
	}
	if err := runVersion([]string{"--nope"}, dir); err == nil {
		t.Fatal("an unknown flag must be a usage error")
	}
}

// TestRunUpgradeCarriesItsFlags proves the parsed flags reach the request the updater
// is asked to verify, and that --check stops before installing.
func TestRunUpgradeCarriesItsFlags(t *testing.T) {
	u := &stubUpdater{plan: upgradePlan(t, "v1.2.0", "v1.3.0-rc.1")}
	withUpdater(t, u)

	if err := runUpgrade([]string{"--to", "v1.3.0-rc.1", "--pre", "--check"}, t.TempDir()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if u.req.To != "v1.3.0-rc.1" || !u.req.AllowPrerelease || u.req.AllowDowngrade {
		t.Fatalf("request = %+v", u.req)
	}
	if u.applied != 0 {
		t.Fatal("--check installed something")
	}
	if err := runUpgrade([]string{"--nope"}, t.TempDir()); err == nil {
		t.Fatal("an unknown flag must be a usage error")
	}
}

// TestPrintBuildNamesWhichBinaryIsRunning: the point of the command is answering "which
// flynn am I actually running", so the path is not optional.
func TestPrintBuildNamesWhichBinaryIsRunning(t *testing.T) {
	var out bytes.Buffer
	if err := printBuild(&out); err != nil {
		t.Fatalf("printBuild: %v", err)
	}
	for _, want := range []string{"version:", "platform:", "go:", "path:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("build report missing %q:\n%s", want, out.String())
		}
	}
}
