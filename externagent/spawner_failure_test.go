package externagent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// unwritableParent returns a workspace path whose parent directory does not exist, so the
// per-run credential home and the per-episode temp directory (both created beside the
// workspace) cannot be created.
func unwritableParent(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-such-parent", "episode")
}

// TestEpisodeAuthHomeReportsAnUncreatableHome proves a credential home that cannot be
// created fails the launch rather than silently pointing the confined child at nothing. A
// child handed no home reports itself logged out on a host where it is logged in, so the
// failure has to be loud.
func TestEpisodeAuthHomeReportsAnUncreatableHome(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{
		AuthDir:       authDirWithToken(t),
		AuthEnv:       "CODEX_HOME",
		AuthSeedFiles: []string{"auth.json"},
	})
	home, seeded, err := sp.episodeAuthHome(unwritableParent(t))
	if err == nil {
		t.Fatal("a credential home that cannot be created must fail the launch")
	}
	if home != "" || len(seeded) != 0 {
		t.Errorf("a failed launch must leave no home and no copies, got home=%q seeded=%v", home, seeded)
	}
	if !strings.Contains(err.Error(), "credential home") {
		t.Errorf("the error should name what could not be created: %v", err)
	}
}

// TestEpisodeAuthHomeCleansUpAfterAFailedCopy proves a seed that cannot be copied leaves no
// half-assembled credential home behind. The copies made before the failure are deleted, so a
// token never sits in a directory the run then abandons.
func TestEpisodeAuthHomeCleansUpAfterAFailedCopy(t *testing.T) {
	src := t.TempDir()
	good := filepath.Join(src, "auth.json")
	if err := os.WriteFile(good, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory in the seed list cannot be read as a file, so the copy fails partway,
	// after the first seed has already landed.
	bad := filepath.Join(src, "config.d")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}

	sp := NewSandboxSpawner(SandboxConfig{AuthEnv: "CLAUDE_CONFIG_DIR", AuthSeedPaths: []string{good, bad}})
	t.Cleanup(func() { _ = sp.Close() })

	home, seeded, err := sp.episodeAuthHome(workdir(t))
	if err == nil {
		t.Fatal("a seed that cannot be copied must fail the launch")
	}
	if home != "" || len(seeded) != 0 {
		t.Errorf("a failed seed must report no home and no copies, got home=%q seeded=%v", home, seeded)
	}
	// The run home itself survives (it is the run's, created on first use), but the token
	// copied into it before the failure does not.
	if sp.runHome == "" {
		t.Fatal("the run home was never created")
	}
	if _, err := os.Stat(filepath.Join(sp.runHome, "auth.json")); err == nil {
		t.Error("a credential copied before the failure was left behind in an abandoned home")
	}
}

// TestAssembleSeedHomeFailures proves the multi-source assembly refuses rather than
// half-building a credential home: a parent it cannot create under fails, and a source that
// cannot be read takes the whole directory with it, so no partially-seeded home is handed to
// an untrusted harness.
func TestAssembleSeedHomeFailures(t *testing.T) {
	if _, err := assembleSeedHome(filepath.Join(t.TempDir(), "no-such-parent"), nil); err == nil {
		t.Error("a home that cannot be created must fail")
	}

	parent := t.TempDir()
	src := t.TempDir()
	dir := filepath.Join(src, "a-directory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := assembleSeedHome(parent, []string{dir})
	if err == nil {
		t.Fatal("a source that cannot be read must fail the assembly")
	}
	if home != "" {
		t.Errorf("a failed assembly must return no home, got %q", home)
	}
	// Nothing is left on disk: the partially-built home was removed.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed assembly left %d entr(ies) behind in %s", len(entries), parent)
	}
}

// TestCopyIfPresentSkipsWhatIsNotThere proves a source that does not exist is skipped rather
// than fabricated, and that a source that cannot be read is reported. A CLI that has never
// been configured has no config file, and whether it is authenticated at all is the
// adapter's question to answer through a probe, not a launch precondition.
func TestCopyIfPresentSkipsWhatIsNotThere(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "copy.json")
	if err := copyIfPresent(filepath.Join(t.TempDir(), "absent.json"), dst); err != nil {
		t.Errorf("a missing source must be skipped, not fail: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a missing source was fabricated at the destination")
	}
	if err := copyIfPresent(t.TempDir(), dst); err == nil {
		t.Error("a source that cannot be read as a file must be reported")
	}
}

// TestCloseWithoutARunHomeIsANoOp proves a run whose episodes never seeded a credential home
// (a detection-only spawner) closes to nothing, and that closing twice does not try to remove
// a directory the run no longer owns.
func TestCloseWithoutARunHomeIsANoOp(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{AuthDir: t.TempDir(), AuthEnv: "CODEX_HOME"})
	if err := sp.Close(); err != nil {
		t.Fatalf("a spawner that seeded nothing must close cleanly: %v", err)
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("a second Close must be a no-op: %v", err)
	}
}

// TestStartRefusesWhatItCannotPrepare proves an episode whose credential home or scratch
// directory cannot be prepared is refused before anything is launched. A child started
// without them would either report itself logged out or die silently when its runtime found
// no writable temp, and the episode would produce no line at all.
func TestStartRefusesWhatItCannotPrepare(t *testing.T) {
	t.Run("a seed name that is a path", func(t *testing.T) {
		sp := NewSandboxSpawner(SandboxConfig{
			AuthDir:       authDirWithToken(t),
			AuthEnv:       "CODEX_HOME",
			AuthSeedFiles: []string{filepath.Join("..", "escape.json")},
		})
		proc, err := sp.Start(context.Background(), Episode{Workdir: workdir(t)}, helperInvocation(0))
		if err == nil {
			if proc != nil {
				_ = proc.Wait()
			}
			t.Fatal("a traversing seed name must refuse the launch")
		}
		if proc != nil {
			t.Fatal("a refused start must not return a process")
		}
	})

	t.Run("no writable scratch beside the workspace", func(t *testing.T) {
		sp := NewSandboxSpawner(SandboxConfig{})
		proc, err := sp.Start(context.Background(), Episode{Workdir: unwritableParent(t)}, helperInvocation(0))
		if err == nil {
			if proc != nil {
				_ = proc.Wait()
			}
			t.Fatal("an episode with nowhere to put its scratch must be refused")
		}
		if !strings.Contains(err.Error(), "temp dir") {
			t.Errorf("the error should name what could not be prepared: %v", err)
		}
		if proc != nil {
			t.Fatal("a refused start must not return a process")
		}
	})
}

// TestProbeRunsUnderTheAssembledCredentialHome proves a detection probe on a split-layout CLI
// sees the same credential home an episode would: the seed paths are gathered into one
// directory and the CLI is pointed at it. Without this the auth-status probe would report a
// logged-in CLI as logged out, because the confined child inherits no home to derive the
// location from.
func TestProbeRunsUnderTheAssembledCredentialHome(t *testing.T) {
	src := t.TempDir()
	cfg := filepath.Join(src, ".claude.json")
	if err := os.WriteFile(cfg, []byte(`{"config":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sp := NewSandboxSpawner(SandboxConfig{AuthEnv: "CLAUDE_CONFIG_DIR", AuthSeedPaths: []string{cfg}})

	name, args := probeArgv(0)
	out, err := sp.Probe(context.Background(), name, args...)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !strings.Contains(out, "probe-ok") {
		t.Fatalf("probe output = %q, want it to contain probe-ok", out)
	}
}

// TestProbeReportsASourceItCannotGather proves a probe whose credential home cannot be
// assembled fails rather than running the CLI against a home that is missing the files
// detection is about to read.
func TestProbeReportsASourceItCannotGather(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{AuthEnv: "CLAUDE_CONFIG_DIR", AuthSeedPaths: []string{t.TempDir()}})
	name, args := probeArgv(0)
	if _, err := sp.Probe(context.Background(), name, args...); err == nil {
		t.Fatal("a credential home that cannot be assembled must fail the probe")
	}
}

// TestProbeGrantsTheHostAuthDirReadOnly proves the read-only detection path is intact: with
// no seeding configured the child is pointed straight at the host's own credential home, so
// an auth-status probe can see whether the CLI is logged in without anything being copied.
func TestProbeGrantsTheHostAuthDirReadOnly(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{AuthDir: authDirWithToken(t), AuthEnv: "CODEX_HOME"})
	name, args := probeArgv(0)
	out, err := sp.Probe(context.Background(), name, args...)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !strings.Contains(out, "probe-ok") {
		t.Fatalf("probe output = %q", out)
	}
}

// TestProbeReportsAnUnrunnableProgram proves a probe of a program that cannot be executed is
// an error, which detection reads as "not present" or "not ready" rather than as a healthy
// CLI that answered nothing.
func TestProbeReportsAnUnrunnableProgram(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{})
	if _, err := sp.Probe(context.Background(), filepath.Join(t.TempDir(), "no-such-program")); err == nil {
		t.Fatal("probing a program that does not exist must be an error")
	}
}

// TestEpisodeOptionsRecordTheHostReadableTierApart pins the one deliberate weakening of the
// episode's confinement. A CLI whose runtime cannot start under a deny-by-default read
// posture is given a host-readable tier. The weakening is real (the harness can read the host
// user's files) and bounded (it can still write nothing outside the workspace, and its egress
// is still gated), so it is named per backend rather than defaulted, and the tier that
// actually ran is what the record names. Where the platform confines reads differently
// (the two Windows tiers bound a code-execution exploit alike but not reads), the two
// configurations must name different tiers, or a record could not tell a reader which held.
func TestEpisodeOptionsRecordTheHostReadableTierApart(t *testing.T) {
	home := t.TempDir()
	confined := NewSandboxSpawner(SandboxConfig{})
	relaxed := NewSandboxSpawner(SandboxConfig{HostReadable: true})

	// The weakening is bounded on every platform: the host's own credential home is never
	// made writable to an untrusted harness, whichever read posture it runs under.
	for name, opts := range map[string][]sandbox.LocalOption{
		"confined": confined.episodeOptions(home, false),
		"relaxed":  relaxed.episodeOptions(home, false),
	} {
		if got := writableDirsOf(t, opts); len(got) != 0 {
			t.Errorf("%s: the host credential home must never be granted write, got %q", name, got)
		}
	}

	strict := tierOf(t, confined.episodeOptions(home, false))
	loose := tierOf(t, relaxed.episodeOptions(home, false))
	if runtime.GOOS != "windows" {
		t.Skipf("this platform confines reads the same way under both tiers (%q); there is nothing for the record to tell apart", strict)
	}
	if strict == loose {
		t.Errorf("both configurations record the tier %q, so a record cannot say which read posture held", strict)
	}
}

// tierOf builds a sandbox from the options and reports the confinement tier a run would
// record for it, read back through the sandbox's own audit accessor.
func tierOf(t *testing.T, opts []sandbox.LocalOption) string {
	t.Helper()
	loc, err := sandbox.NewLocal(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = loc.Close() })
	return loc.ConfinementTier()
}
