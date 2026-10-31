package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/spine"
)

// stubHarnessDriver stands in for an external-agent driver. It reports only what its fields
// say, so a test can hand the host a harness that tallies provenance, one that records
// attested events, one that does neither, and one that fails to close.
type stubHarnessDriver struct {
	name       string
	tiers      map[externagent.Tier]int
	steering   externagent.Steering
	drift      map[string]int
	recorder   externagent.Recorder
	unrecorded int
	unrecErr   error
	closed     bool
}

func (d *stubHarnessDriver) Name() string { return d.name }

func (d *stubHarnessDriver) Build(driver.Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	return nil, nil, errors.New("the stub driver builds no loop")
}

// plainDriver is a harness that reports nothing about itself: no tiers, no recorder, no
// closer. The host must still be able to name it.
type plainDriver struct{ stubHarnessDriver }

// tallyingDriver reports the provenance of what its episodes projected.
type tallyingDriver struct{ stubHarnessDriver }

func (d *tallyingDriver) Tiers() map[externagent.Tier]int    { return d.tiers }
func (d *tallyingDriver) Steering() externagent.Steering     { return d.steering }
func (d *tallyingDriver) Drift() map[string]int              { return d.drift }
func (d *tallyingDriver) SetRecorder(r externagent.Recorder) { d.recorder = r }
func (d *tallyingDriver) Unrecorded() (int, error)           { return d.unrecorded, d.unrecErr }
func (d *tallyingDriver) Close() error {
	d.closed = true
	return nil
}

// TestObservedProvenanceReadsTheHarnessAccount checks what the host can say about an
// external run once it has finished: the tally of attested events, how often the harness
// reached for its own tools instead of the governed bridge, and where it drifted from the
// session contract.
func TestObservedProvenanceReadsTheHarnessAccount(t *testing.T) {
	d := &tallyingDriver{stubHarnessDriver{
		name:  "codex",
		tiers: map[externagent.Tier]int{externagent.TierAttested: 7},
		// Four calls in total, one of them run with the harness's own tools.
		steering: externagent.Steering{BridgeCalls: 3, NativeCommands: 1},
		drift:    map[string]int{"ignored_tool": 2},
	}}
	got := observedProvenance(&externAgent{driver: d})
	if got.harness != "codex" {
		t.Errorf("harness = %q, want the driver's name", got.harness)
	}
	if got.attested != 7 {
		t.Errorf("attested = %d, want the harness's attested tier count", got.attested)
	}
	if got.nativeRate != 0.25 {
		t.Errorf("nativeRate = %v, want a quarter of the calls counted as native", got.nativeRate)
	}
	if got.drift["ignored_tool"] != 2 {
		t.Errorf("drift = %v, want the contract drift carried through", got.drift)
	}
}

// TestObservedProvenanceOfASilentHarness checks a driver that reports none of this still
// yields a declaration naming the run as externally driven, with nothing invented.
func TestObservedProvenanceOfASilentHarness(t *testing.T) {
	got := observedProvenance(&externAgent{driver: &plainDriver{stubHarnessDriver{name: "claude"}}})
	if got.harness != "claude" {
		t.Errorf("harness = %q", got.harness)
	}
	if got.attested != 0 || got.nativeRate != 0 || got.drift != nil {
		t.Errorf("a silent harness must report nothing rather than a guess, got %+v", got)
	}
}

// TestRecordAttestedEventsBindsTheStream checks the recording binding: a driver that can
// record is given the run's stream, and one that cannot is left alone and reports no
// unrecorded events, so a count is never declared over an account that was never bound.
func TestRecordAttestedEventsBindsTheStream(t *testing.T) {
	d := &tallyingDriver{stubHarnessDriver{name: "codex", unrecorded: 3}}
	ea := &externAgent{driver: d}
	log := spine.NewMemoryLog()
	governed.RecordAttested(ea.driver, log, "run-1")
	if d.recorder == nil {
		t.Fatal("the driver was not given a recorder")
	}
	if err := d.recorder.Record(context.Background(), externagent.Event{Kind: externagent.EventText, Tier: externagent.TierAttested, Raw: []byte(`{}`)}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if evs, err := log.Read(context.Background(), spine.Query{Stream: "run-1"}); err != nil || len(evs) != 1 {
		t.Fatalf("the driver was not bound to the run's stream: %d events, %v", len(evs), err)
	}
	n, err := unrecordedAttested(ea)
	if n != 3 || err != nil {
		t.Errorf("unrecordedAttested = (%d, %v), want the driver's own count", n, err)
	}

	d.unrecErr = errors.New("the record was closed mid-episode")
	if _, err := unrecordedAttested(ea); err == nil {
		t.Error("a driver that cannot report its unrecorded events must surface the failure")
	}

	plain := &externAgent{driver: &plainDriver{stubHarnessDriver{name: "other"}}}
	governed.RecordAttested(plain.driver, nil, "run-2") // must not panic on a driver with no recorder
	if n, err := unrecordedAttested(plain); n != 0 || err != nil {
		t.Errorf("a non-recording driver must report no gap, got (%d, %v)", n, err)
	}
}

// TestExternAgentCloseReleasesTheHarnessHome checks the teardown every caller defers: a
// driver that holds a credential home is closed, a driver that holds nothing is not asked to,
// and a nil agent (a native run) closes to nothing.
func TestExternAgentCloseReleasesTheHarnessHome(t *testing.T) {
	d := &tallyingDriver{stubHarnessDriver{name: "codex"}}
	ea := &externAgent{driver: d}
	ea.close()
	if !d.closed {
		t.Error("a closable driver must be closed when the run that resolved it is over")
	}

	(&externAgent{driver: &plainDriver{stubHarnessDriver{name: "other"}}}).close()
	var none *externAgent
	none.close()
}

// TestExternalBackendAssemblyRefusesAnUnknownName checks a misnamed backend fails at
// assembly, in every place a backend is built, rather than running the wrong loop.
func TestExternalBackendAssemblyRefusesAnUnknownName(t *testing.T) {
	if _, err := externalSpawner("nope"); err == nil || !strings.Contains(err.Error(), "unknown external agent backend") {
		t.Errorf("externalSpawner error = %v, want an unknown-backend refusal", err)
	}
	if _, err := externalAdapter("nope", nil); err == nil || !strings.Contains(err.Error(), "unknown external agent backend") {
		t.Errorf("externalAdapter error = %v, want an unknown-backend refusal", err)
	}
	if _, err := resolveExternalAgent(context.Background(), "nope", "", t.TempDir()); err == nil {
		t.Error("resolving an unknown backend must fail before anything is spawned")
	}
}

// TestExternalAdapterIsBuiltForEveryBackend checks each known backend assembles an adapter
// and a spawner with the CLI absent: detection reports a missing install as onboarding, so
// assembly must not fail on it. The PATH is emptied so the result does not depend on what
// this machine has installed.
func TestExternalAdapterIsBuiltForEveryBackend(t *testing.T) {
	withNoExecutables(t)
	for _, name := range externalAgentNames() {
		spawner, err := externalSpawner(name)
		if err != nil {
			t.Fatalf("externalSpawner(%s): %v", name, err)
		}
		if spawner == nil {
			t.Fatalf("externalSpawner(%s) returned no spawner", name)
		}
		adapter, err := externalAdapter(name, spawner)
		if err != nil {
			t.Fatalf("externalAdapter(%s): %v", name, err)
		}
		if adapter == nil {
			t.Fatalf("externalAdapter(%s) returned no adapter", name)
		}
	}
}

// TestCodexAuthDirFollowsTheCLIsOwnResolution checks the credential home granted to the
// confined child: CODEX_HOME wins, and the default is the CLI's own home directory.
func TestCodexAuthDirFollowsTheCLIsOwnResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if got := codexAuthDir(); got != home {
		t.Errorf("codexAuthDir = %q, want the CODEX_HOME override", got)
	}

	t.Setenv("CODEX_HOME", "")
	got := codexAuthDir()
	if got != "" && filepath.Base(got) != ".codex" {
		t.Errorf("codexAuthDir = %q, want the CLI's default home", got)
	}
}

// TestClaudeHomeIsFlynnsOwnAndNeverTheUsers checks the claude backend runs the CLI against
// the home Flynn keeps for it, whatever the user's own CLAUDE_CONFIG_DIR says, and that the
// home exists once the spawner is built so detection can be granted read on it before the
// user has signed in.
func TestClaudeHomeIsFlynnsOwnAndNeverTheUsers(t *testing.T) {
	withNoExecutables(t)
	home := filepath.Join(t.TempDir(), "claude")
	prev := claudeHome
	claudeHome = func() string { return home }
	t.Cleanup(func() { claudeHome = prev })
	userDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)

	if _, err := externalSpawner("claude"); err != nil {
		t.Fatalf("externalSpawner(claude): %v", err)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Fatalf("the claude home must exist after the spawner is built: %v", err)
	}
	def := defaultClaudeHome()
	if filepath.Dir(def) != defaultDataDir() {
		t.Errorf("the default claude home %q must sit under the data directory", def)
	}
	if def == userDir {
		t.Errorf("the claude home must not follow the user's CLAUDE_CONFIG_DIR, got %q", def)
	}
}

// TestClaudeLoginSignsInFlynnsHome checks a not-signed-in claude is pointed at a login that
// signs in Flynn's home through the CLI's own subscription flow, and that the other
// readiness states pass through untouched.
func TestClaudeLoginSignsInFlynnsHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "it's here")
	out := withClaudeLogin(externagent.Readiness{Available: true, Reason: "run `claude auth login`"}, home)
	for _, want := range []string{"CLAUDE_CONFIG_DIR", "claude auth login --claudeai", "separate from yours"} {
		if !strings.Contains(out.Reason, want) {
			t.Errorf("login reason %q lacks %q", out.Reason, want)
		}
	}
	if strings.Contains(strings.ToLower(out.Reason), "api key") {
		t.Errorf("the login must never ask for an API key, got %q", out.Reason)
	}
	cmd := claudeLoginCommand(home)
	if runtime.GOOS == "windows" {
		if !strings.Contains(cmd, "it''s here") || !strings.HasSuffix(cmd, "Remove-Item Env:CLAUDE_CONFIG_DIR") {
			t.Errorf("PowerShell login must quote the path and clear the variable, got %q", cmd)
		}
	} else if !strings.Contains(cmd, `it'\''s here`) {
		t.Errorf("sh login must quote the path, got %q", cmd)
	}

	for _, r := range []externagent.Readiness{
		{Available: false, Reason: "install it"},
		{Available: true, Refuse: true, Reason: "update it"},
		{Available: true, LoggedIn: true},
	} {
		if got := withClaudeLogin(r, home); got != r {
			t.Errorf("withClaudeLogin(%+v) = %+v, want it unchanged", r, got)
		}
	}
}

// TestCodexSeedCarriesTheCredentialAndNoOperatorConfig is the host half of the codex
// adapter's declared host-config posture. The adapter states that an episode's child runs
// under a per-episode home holding the credential and nothing else; the seed list is where
// that is either true or quietly false. config.toml is the file that would break it: it is
// where a codex user keeps their MCP servers, model providers and settings, so seeding it
// would hand the harness servers this run neither hosts nor governs and let the launching
// operator's configuration steer the episode.
func TestCodexSeedCarriesTheCredentialAndNoOperatorConfig(t *testing.T) {
	if len(codexAuthSeedFiles) != 1 || codexAuthSeedFiles[0] != "auth.json" {
		t.Fatalf("codexAuthSeedFiles = %v, want only the credential", codexAuthSeedFiles)
	}
	for _, f := range codexAuthSeedFiles {
		if f == "config.toml" {
			t.Error("the operator's codex config must not be seeded into an episode's home")
		}
	}
	if got := externagent.NewCodex("codex", nil).Lockdown().HostConfig; !got.Stripped() {
		t.Errorf("codex host-config posture = %v, want a stripped one to match the seed", got)
	}
}

// TestResolveExternalAgentReportsAMissingCLI checks the onboarding path for an external
// backend: with the CLI nowhere on the PATH, resolution surfaces the CLI's own actionable
// reason (install it) rather than a raw error, and never asks for an API key, since an
// external agent authenticates on its owner's subscription.
func TestResolveExternalAgentReportsAMissingCLI(t *testing.T) {
	// An empty PATH, and a credential home with nothing in it, is a machine where the CLI is
	// simply not installed.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())

	for _, name := range externalAgentNames() {
		ea, err := resolveExternalAgent(context.Background(), name, "", t.TempDir())
		if err == nil {
			t.Fatalf("%s: an uninstalled CLI must not resolve to a driver", name)
		}
		if ea != nil {
			t.Errorf("%s: no agent may be returned alongside a failure", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, name) {
			t.Errorf("%s: the failure must name the backend, got %v", name, err)
		}
		if !strings.Contains(msg, "install") {
			t.Errorf("%s: the failure must name the next step, got %v", name, err)
		}
		if strings.Contains(strings.ToLower(msg), "api key") {
			t.Errorf("%s: external onboarding must never ask for an API key, got %v", name, err)
		}
	}
}
