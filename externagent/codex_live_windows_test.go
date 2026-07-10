//go:build windows

package externagent

import (
	"context"
	"strings"
	"testing"
	"time"
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
