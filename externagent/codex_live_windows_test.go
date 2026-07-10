//go:build windows

package externagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

// TestLiveCodexBootsUnderWriteRestrictedTier runs the real installed codex CLI under the
// exact production spawner envelope cmd/flynn builds (HostReadable set). It is the
// regression guard for the defect that blocked every live codex run on Windows: the CLI
// is a Rust program that canonicalizes a path on startup, which the AppContainer tier's
// token cannot do, so it died before main. Under the write-restricted tier it must reach
// main and report its version. The test skips where codex is not installed (CI), so it is
// a local integration check rather than a hermetic unit test.
func TestLiveCodexBootsUnderWriteRestrictedTier(t *testing.T) {
	prog, err := LocateCodex("")
	if err != nil {
		t.Skipf("codex not installed: %v", err)
	}
	t.Logf("codex at %s", prog.Path)
	sp := NewSandboxSpawner(SandboxConfig{
		AuthEnv:      "CODEX_HOME",
		ProgramDirs:  prog.ReadableDirs,
		ProbeTimeout: 30 * time.Second,
		HostReadable: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := sp.Probe(ctx, prog.Path, "--version")
	t.Logf("probe err=%v out=%q", err, out)
	if err != nil {
		t.Fatalf("live codex --version failed under the write-restricted tier: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("empty version output")
	}
}

// liveCodexHome resolves the installed codex CLI's credential home, skipping the test
// when the CLI has never been logged in: without a token no episode starts, and the write
// behaviour this test measures never happens.
func liveCodexHome(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CODEX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home directory: %v", err)
		}
		dir = filepath.Join(home, ".codex")
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
		t.Skipf("codex is not logged in at %s: %v", dir, err)
	}
	return dir
}

// dirEntries lists a directory's immediate entries, for comparing the host credential
// home before and after an episode.
func dirEntries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]bool{}
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out
}

// runLiveCodex runs one real codex turn against the given credential home under the
// write-restricted tier, the same confinement a live episode uses, and returns its
// combined output. Governed egress is deliberately left out: the Windows egress waist
// cannot enforce a policy yet, so SandboxSpawner.Start refuses on this platform and the
// full episode path is proven on the posix leg instead. Everything this test measures
// (whether the CLI can write its own home) happens at startup, under the same tier and
// the same grants, before any model call.
func runLiveCodex(t *testing.T, prog Program, home string, writable bool) string {
	t.Helper()
	workdir := t.TempDir()
	opts := []sandbox.LocalOption{
		sandbox.WithDefaultConfinement(),
		sandbox.WithHostReadable(),
		sandbox.WithReadableDir(prog.ReadableDirs...),
		sandbox.WithReadableDir(home),
		sandbox.WithExecTimeout(60 * time.Second),
	}
	if writable {
		opts = append(opts, sandbox.WithWritableDir(home))
	}
	loc, err := sandbox.NewLocal(workdir, opts...)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = loc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := loc.Capture(ctx, sandbox.CaptureSpec{
		Argv: []string{prog.Path, "exec", "--skip-git-repo-check", "reply with the single word OK"},
		Env:  []string{"CODEX_HOME=" + home},
	})
	if err != nil {
		t.Fatalf("run codex: %v", err)
	}
	t.Logf("writable=%v exit=%d out=%q", writable, res.ExitCode, res.Output)
	return res.Output
}

// seedLiveCodexHome builds a per-episode credential home from the real one, exactly as an
// episode does, and returns it.
func seedLiveCodexHome(t *testing.T, auth string) string {
	t.Helper()
	sp := NewSandboxSpawner(SandboxConfig{
		AuthDir:       auth,
		AuthEnv:       "CODEX_HOME",
		AuthSeedFiles: []string{"auth.json", "config.toml"},
	})
	home, owned, err := sp.episodeAuthHome(filepath.Join(t.TempDir(), "episode"))
	if err != nil || !owned {
		t.Fatalf("seed credential home: owned=%v err=%v", owned, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// TestLiveCodexNeedsWritableCredentialHome answers, against the installed CLI, the
// question the design turned on: does a real codex turn have to write CODEX_HOME? It runs
// the same turn twice under the same tier, once with the home read-only and once with it
// granted write, and asserts the read-only run fails to write while the granted run
// succeeds. If codex ever stops needing the write, the first half of this test starts
// passing where it should fail, and the grant can be removed rather than carried forever
// on a stale assumption.
func TestLiveCodexNeedsWritableCredentialHome(t *testing.T) {
	prog, err := LocateCodex("")
	if err != nil {
		t.Skipf("codex not installed: %v", err)
	}
	auth := liveCodexHome(t)

	readonly := runLiveCodex(t, prog, seedLiveCodexHome(t, auth), false)
	if !strings.Contains(readonly, "could not update PATH") {
		t.Fatalf("codex no longer reports a write failure against a read-only credential home; if it needs no write, drop WithWritableDir from the episode: %s", readonly)
	}

	home := seedLiveCodexHome(t, auth)
	granted := runLiveCodex(t, prog, home, true)
	if strings.Contains(granted, "could not update PATH") {
		t.Fatalf("codex still cannot write its credential home even when granted: %s", granted)
	}
	// Not merely quiet: the CLI got past the rollout recorder the read-only run died on,
	// which it can only do by writing a session file into the home. Whether the model call
	// that follows succeeds depends on the account's entitlements, not on this confinement
	// (an unconfined run of the same command answers the same way), so the turn's outcome
	// is deliberately not asserted here.
	if !strings.Contains(granted, "session id:") {
		t.Fatalf("codex did not open a session against the granted credential home: %s", granted)
	}
	if len(dirEntries(t, home)) <= 2 {
		t.Fatalf("codex wrote nothing into the granted credential home %s", home)
	}
}

// TestLiveCodexLeavesHostCredentialHomeUntouched proves the per-episode copy is what
// absorbs those writes: an untrusted harness runs a real turn and the host's own codex
// home gains nothing, so its token and history are beyond the harness's reach.
func TestLiveCodexLeavesHostCredentialHomeUntouched(t *testing.T) {
	prog, err := LocateCodex("")
	if err != nil {
		t.Skipf("codex not installed: %v", err)
	}
	auth := liveCodexHome(t)
	before := dirEntries(t, auth)
	runLiveCodex(t, prog, seedLiveCodexHome(t, auth), true)
	for name := range dirEntries(t, auth) {
		if !before[name] {
			t.Fatalf("the confined turn created %q in the host credential home %s", name, auth)
		}
	}
}
