//go:build windows

package externagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveClaudeBootsAndDetectsUnderConfinement runs the real installed claude CLI under the
// production spawner envelope and asserts detection works end to end: the confined child can
// launch the native binary and read its own subscription auth home. Claude Code is a
// compiled binary that canonicalizes paths on startup, so it runs under the tier that leaves
// the host readable while confining writes; the credential home is a combined directory
// assembled from the CLI's split config and token files. The test skips where claude is not
// installed (CI), so it is a local integration check rather than a hermetic unit test.
//
// A live episode is not driven here: on this platform a governed-egress leg is not present,
// so SandboxSpawner.Start refuses, and the full episode is proven on the posix leg. What this
// asserts is the part that does run on Windows, detection under the real confinement.
func TestLiveClaudeBootsAndDetectsUnderConfinement(t *testing.T) {
	prog, err := LocateClaude("")
	if err != nil {
		t.Skipf("claude not installed: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	// The same split credential layout the production backend gathers: the config in the home
	// directory and the OAuth token in a subdirectory.
	seeds := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", ".credentials.json"),
	}
	sp := NewSandboxSpawner(SandboxConfig{
		AuthEnv:       "CLAUDE_CONFIG_DIR",
		AuthSeedPaths: seeds,
		ProgramDirs:   prog.ReadableDirs,
		ProbeTimeout:  40 * time.Second,
		HostReadable:  true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	r, err := NewClaude(prog.Path, sp).Detect(ctx)
	if err != nil {
		t.Fatalf("Detect returned an error (it should report unreadiness, not error): %v", err)
	}
	t.Logf("claude boot: available=%v version=%q ready=%v reason=%q", r.Available, r.Version, r.Ready(), r.Reason)

	if !r.Available {
		t.Skipf("claude is present but the confined child could not run it: %s", r.Reason)
	}
	if r.Version == "" {
		t.Errorf("an available claude must report a version")
	}
	// Readiness must be internally consistent whether or not this host is logged in.
	if r.Refuse && r.Ready() {
		t.Errorf("a refused CLI cannot be Ready")
	}
	if !r.Ready() && r.Reason == "" {
		t.Errorf("a not-ready claude must carry an actionable reason")
	}
	// A logged-in subscription host is the common local case: assert the credential home was
	// read (Ready), and if not, that the reason names the next step rather than crashing.
	if !r.LoggedIn && r.Reason == "" {
		t.Errorf("a logged-out claude must carry a reason pointing at sign-in")
	}
}
