package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/goal"
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
	recordAttestedEvents(ea, nil, "run-1")
	sink, ok := d.recorder.(*attestedSink)
	if !ok || sink.stream != "run-1" {
		t.Fatalf("the driver was not bound to the run's stream, got %#v", d.recorder)
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
	recordAttestedEvents(plain, nil, "run-2") // must not panic on a driver with no recorder
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
// and a spawner, whether or not the CLI is installed on this machine: detection reports a
// missing install as onboarding, so assembly must not fail on it.
func TestExternalAdapterIsBuiltForEveryBackend(t *testing.T) {
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

// TestClaudeSeedPathsGatherBothCredentialFiles checks the two files the confined child needs
// to authenticate are seeded by base name into one home: with the config dir overridden they
// are both taken from it, flat, matching where the CLI looks for them.
func TestClaudeSeedPathsGatherBothCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	got := claudeSeedPaths()
	want := []string{filepath.Join(dir, ".claude.json"), filepath.Join(dir, ".credentials.json")}
	if len(got) != len(want) {
		t.Fatalf("claudeSeedPaths = %v, want both credential files", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("seed path %d = %q, want %q", i, got[i], want[i])
		}
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	def := claudeSeedPaths()
	if len(def) != 2 {
		t.Fatalf("the default must still seed both files, got %v", def)
	}
	if filepath.Base(def[0]) != ".claude.json" || filepath.Base(def[1]) != ".credentials.json" {
		t.Errorf("the default seed paths name the wrong files: %v", def)
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

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
