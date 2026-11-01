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

// TestCodexHomeIsFlynnsOwnAndNeverTheUsers checks the codex backend runs the CLI against the
// home Flynn keeps for it, whatever the user's own CODEX_HOME says, that the home exists once
// the spawner is built (the CLI refuses a CODEX_HOME that does not), and that a config.toml,
// AGENTS.md or skills directory left in it is gone before any episode can read it while the sign-in beside it is kept.
func TestCodexHomeIsFlynnsOwnAndNeverTheUsers(t *testing.T) {
	withNoExecutables(t)
	home := filepath.Join(t.TempDir(), "codex")
	prev := codexHome
	codexHome = func() string { return home }
	t.Cleanup(func() { codexHome = prev })
	userDir := t.TempDir()
	t.Setenv("CODEX_HOME", userDir)

	if _, err := externalSpawner("codex"); err != nil {
		t.Fatalf("externalSpawner(codex): %v", err)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Fatalf("the codex home must exist after the spawner is built: %v", err)
	}
	for name, body := range map[string]string{"config.toml": "[mcp_servers.x]\n", "auth.json": "{}"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := externalSpawner("codex"); err != nil {
		t.Fatalf("externalSpawner(codex): %v", err)
	}
	for _, name := range []string{"config.toml", "AGENTS.md", "skills"} {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s in Flynn's codex home must be removed before an episode, got %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Errorf("the sign-in in Flynn's codex home must be left alone: %v", err)
	}
	def := defaultCodexHome()
	if filepath.Dir(def) != defaultDataDir() {
		t.Errorf("the default codex home %q must sit under the data directory", def)
	}
	if def == userDir {
		t.Errorf("the codex home must not follow the user's CODEX_HOME, got %q", def)
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

// TestLoginSignsInFlynnsHome checks a not-signed-in claude or codex is pointed at a login
// that signs in Flynn's home for that CLI through the CLI's own subscription flow, and that
// the other readiness states pass through untouched.
func TestLoginSignsInFlynnsHome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		home  *func() string
		env   string
		login string
	}{
		{"claude", &claudeHome, "CLAUDE_CONFIG_DIR", "claude auth login --claudeai"},
		{"codex", &codexHome, "CODEX_HOME", "codex login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "it's here")
			prev := *tc.home
			*tc.home = func() string { return home }
			t.Cleanup(func() { *tc.home = prev })

			out := withHomeLogin(tc.name, externagent.Readiness{Available: true, Reason: "run the CLI's login"})
			for _, want := range []string{tc.env, tc.login, "separate from yours"} {
				if !strings.Contains(out.Reason, want) {
					t.Errorf("login reason %q lacks %q", out.Reason, want)
				}
			}
			if strings.Contains(strings.ToLower(out.Reason), "api key") {
				t.Errorf("the login must never ask for an API key, got %q", out.Reason)
			}
			cmd := homeLoginCommand(tc.env, home, tc.login)
			if runtime.GOOS == "windows" {
				if !strings.Contains(cmd, "it''s here") || !strings.HasSuffix(cmd, "Remove-Item Env:"+tc.env) {
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
				if got := withHomeLogin(tc.name, r); got != r {
					t.Errorf("withHomeLogin(%+v) = %+v, want it unchanged", r, got)
				}
			}
		})
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

// TestClearOwnHomeReportsAnUnreadableHome checks a home that cannot be listed for a reason
// other than being absent is an error rather than a silent pass, since a config left in it
// would then steer the next episode; an absent home has nothing to clear.
func TestClearOwnHomeReportsAnUnreadableHome(t *testing.T) {
	if err := clearOwnHome("codex", filepath.Join(t.TempDir(), "absent"), codexAuthFiles); err != nil {
		t.Errorf("an absent home has nothing to clear, got %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports listing a file as a missing path")
	}
	file := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearOwnHome("codex", file, codexAuthFiles); err == nil || !strings.Contains(err.Error(), "codex home") {
		t.Errorf("a home that is a file must be reported, got %v", err)
	}
}

// TestClearOwnHomeReportsAnUnremovableEntry checks a leftover the process cannot delete is
// reported: a read-only directory's contents cannot be unlinked on unix.
func TestClearOwnHomeReportsAnUnremovableEntry(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not block removal here")
	}
	home := t.TempDir()
	sub := filepath.Join(home, "skills")
	if err := os.MkdirAll(filepath.Join(sub, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	if err := clearOwnHome("codex", home, codexAuthFiles); err == nil {
		t.Error("an entry that cannot be removed must be reported")
	}
}

// TestExternalHomesAreOwnerOnly checks an existing home left with a wider mode is tightened
// to 0700 when the spawner is built, since MkdirAll leaves it as it found it.
func TestExternalHomesAreOwnerOnly(t *testing.T) {
	withNoExecutables(t)
	for _, tc := range []struct {
		name string
		home *func() string
	}{{"claude", &claudeHome}, {"codex", &codexHome}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tc.name)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			prev := *tc.home
			*tc.home = func() string { return dir }
			t.Cleanup(func() { *tc.home = prev })
			if _, err := externalSpawner(tc.name); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS == "windows" {
				t.Skip("Windows permission bits do not express owner-only access")
			}
			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o700 {
				t.Errorf("home mode = %v, want 0700", fi.Mode().Perm())
			}
		})
	}
}

// TestEnsureOwnHomeReportsAFailure checks a home that cannot be created is an error that
// names the backend.
func TestEnsureOwnHomeReportsAFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnHome("claude", filepath.Join(file, "home")); err == nil || !strings.Contains(err.Error(), "claude home") {
		t.Errorf("got %v", err)
	}
}

// TestOwnHomeSpawnersGrantWriteOnTheirHome checks the claude and codex envelopes mark their
// home writable and point the CLI at it. Both CLIs refresh their token in place, so a read-only
// home would sign Flynn out at the first refresh; the grant is safe only because the home is
// Flynn's own, which is why it must not be dropped or copied to a backend that uses the
// user's.
func TestOwnHomeSpawnersGrantWriteOnTheirHome(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		name string
		cfg  externagent.SandboxConfig
		env  string
	}{
		{"claude", claudeSandboxConfig(home, externagent.Program{}), "CLAUDE_CONFIG_DIR"},
		{"codex", codexSandboxConfig(home, externagent.Program{}), "CODEX_HOME"},
	} {
		if !tc.cfg.AuthDirWritable {
			t.Errorf("%s: AuthDirWritable must be true", tc.name)
		}
		if tc.cfg.AuthDir != home || tc.cfg.AuthEnv != tc.env {
			t.Errorf("%s: AuthDir/AuthEnv = %q/%q, want %q/%q", tc.name, tc.cfg.AuthDir, tc.cfg.AuthEnv, home, tc.env)
		}
	}
}

// TestExternalSpawnerRefusesAHomeItCannotPrepare checks a home that cannot be created, or a
// codex home that cannot be cleared, fails the spawner instead of running an episode against
// an unprepared one.
func TestExternalSpawnerRefusesAHomeItCannotPrepare(t *testing.T) {
	withNoExecutables(t)
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		home *func() string
	}{{"claude", &claudeHome}, {"codex", &codexHome}} {
		prev := *tc.home
		*tc.home = func() string { return filepath.Join(file, "home") }
		_, err := externalSpawner(tc.name)
		*tc.home = prev
		if err == nil || !strings.Contains(err.Error(), tc.name+" home") {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	for _, tc := range []struct {
		name string
		home *func() string
	}{{"claude", &claudeHome}, {"codex", &codexHome}} {
		home := t.TempDir()
		sub := filepath.Join(home, "skills")
		if err := os.MkdirAll(filepath.Join(sub, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
		prev := *tc.home
		*tc.home = func() string { return home }
		_, err := externalSpawner(tc.name)
		*tc.home = prev
		if err == nil {
			t.Errorf("a %s home that cannot be cleared must fail the spawner", tc.name)
		}
	}
}

// TestClaudeHomeKeepsOnlyItsSignIn checks everything a harness could leave in Flynn's claude
// home to steer a later run is gone once the spawner is built, while the sign-in survives.
func TestClaudeHomeKeepsOnlyItsSignIn(t *testing.T) {
	withNoExecutables(t)
	home := filepath.Join(t.TempDir(), "claude")
	prev := claudeHome
	claudeHome = func() string { return home }
	t.Cleanup(func() { claudeHome = prev })
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".credentials.json", "settings.json", "CLAUDE.md", ".claude.json", filepath.Join("agents", "x.md")} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := externalSpawner("claude"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "CLAUDE.md", ".claude.json", "agents"} {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s in Flynn's claude home must be removed before an episode, got %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); err != nil {
		t.Errorf("the sign-in in Flynn's claude home must be left alone: %v", err)
	}
}
