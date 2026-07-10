package externagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// authDirWithToken builds a stand-in for an external CLI's credential home holding a
// token, a config file, and a session log the episode must not copy.
func authDirWithToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"auth.json":   `{"token":"secret"}`,
		"config.toml": "model = \"gpt-5-codex\"\n",
		"history.log": "an earlier session",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return dir
}

// workdir returns an episode workspace, whose parent is where the per-episode credential
// home is created.
func workdir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "episode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("workdir: %v", err)
	}
	return dir
}

func TestEpisodeAuthHomeCopiesOnlySeedFiles(t *testing.T) {
	auth := authDirWithToken(t)
	wd := workdir(t)
	sp := NewSandboxSpawner(SandboxConfig{
		AuthDir:       auth,
		AuthEnv:       "CODEX_HOME",
		AuthSeedFiles: []string{"auth.json", "config.toml", "absent.json"},
	})
	home, owned, err := sp.episodeAuthHome(wd)
	if err != nil {
		t.Fatalf("episodeAuthHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if !owned {
		t.Fatalf("a seeded credential home must be owned by the spawner, so it is deleted with the episode")
	}
	if home == auth {
		t.Fatalf("the episode must not be pointed at the host credential home")
	}
	// The workspace is what the record captures, so the copied token must not be inside it.
	if rel, err := filepath.Rel(wd, home); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the credential home %s is inside the recorded workspace %s", home, wd)
	}
	if b, err := os.ReadFile(filepath.Join(home, "auth.json")); err != nil || !strings.Contains(string(b), "secret") {
		t.Fatalf("the token was not seeded into the episode home: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Fatalf("the config was not seeded: %v", err)
	}
	// A file not named as a seed stays in the host home: an untrusted harness gets the
	// credential it needs and none of the history beside it.
	if _, err := os.Stat(filepath.Join(home, "history.log")); err == nil {
		t.Fatalf("an unnamed file was copied into the episode home")
	}
	// A seed file that does not exist is skipped, not fabricated and not fatal.
	if _, err := os.Stat(filepath.Join(home, "absent.json")); err == nil {
		t.Fatalf("a missing seed file was created")
	}
}

// TestEpisodeAuthHomeWithoutSeedFilesUsesAuthDir proves the read-only behaviour is intact
// where no seeding is configured (a detection probe), and that the spawner claims no
// ownership of the host's directory: a stray delete of a real credential home is the worst
// thing this code could do.
func TestEpisodeAuthHomeWithoutSeedFilesUsesAuthDir(t *testing.T) {
	auth := authDirWithToken(t)
	sp := NewSandboxSpawner(SandboxConfig{AuthDir: auth, AuthEnv: "CODEX_HOME"})
	home, owned, err := sp.episodeAuthHome(workdir(t))
	if err != nil {
		t.Fatalf("episodeAuthHome: %v", err)
	}
	if home != auth || owned {
		t.Fatalf("expected the unowned host auth dir, got home=%q owned=%v", home, owned)
	}
}

// TestEpisodeAuthHomeNoAuthDir proves a host with no resolvable credential home yields no
// home and no seeding, rather than an empty directory the CLI would read as logged out
// while the spawner reports it seeded one.
func TestEpisodeAuthHomeNoAuthDir(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{AuthEnv: "CODEX_HOME", AuthSeedFiles: []string{"auth.json"}})
	home, owned, err := sp.episodeAuthHome(workdir(t))
	if err != nil || home != "" || owned {
		t.Fatalf("expected no credential home, got home=%q owned=%v err=%v", home, owned, err)
	}
}

// TestAuthEnvPointsAtEpisodeHome proves the variable the CLI reads carries the episode's
// home rather than the host's, and carries a path rather than the credential itself.
func TestAuthEnvPointsAtEpisodeHome(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{AuthDir: "/host/.codex", AuthEnv: "CODEX_HOME"})
	got := sp.authEnv("/run/episode-home")
	if len(got) != 1 || got[0] != "CODEX_HOME=/run/episode-home" {
		t.Fatalf("authEnv must point the CLI at the episode home, got %q", got)
	}
	if len(sp.authEnv("")) != 0 {
		t.Fatalf("no credential home must pass no variable")
	}
}

// TestEpisodeOptionsGrantsWriteOnlyToOwnedHome is the security property the whole design
// exists for: the host's real credential home is never made writable to an untrusted
// harness, only a per-episode copy is.
func TestEpisodeOptionsGrantsWriteOnlyToOwnedHome(t *testing.T) {
	host := t.TempDir()
	owned := t.TempDir()
	sp := NewSandboxSpawner(SandboxConfig{AuthDir: host, AuthEnv: "CODEX_HOME"})

	if got := writableDirsOf(t, sp.episodeOptions(host, false)); len(got) != 0 {
		t.Fatalf("the host credential home must never be granted write, got %q", got)
	}
	got := writableDirsOf(t, sp.episodeOptions(owned, true))
	if len(got) != 1 || !sameDir(t, got[0], owned) {
		t.Fatalf("the owned episode home must be granted write, got %q want %q", got, owned)
	}
}

// writableDirsOf builds a sandbox from the options and reports the directories they left
// writable, read back through the sandbox's own audit accessor.
func writableDirsOf(t *testing.T, opts []sandbox.LocalOption) []string {
	t.Helper()
	loc, err := sandbox.NewLocal(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	t.Cleanup(func() { _ = loc.Close() })
	return loc.WritableDirs()
}

// sameDir compares two paths after resolving symlinks, because the sandbox stores the
// resolved form and a temp directory on macOS is reached through one.
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// TestEpisodeAuthHomeRejectsPathSeed proves a seed name that is a path, not a file name,
// is refused rather than followed: a traversing name would copy a file from outside the
// credential home into a directory the untrusted harness can read and write.
func TestEpisodeAuthHomeRejectsPathSeed(t *testing.T) {
	for _, bad := range []string{"../secret", filepath.Join("nested", "auth.json"), "..", "."} {
		sp := NewSandboxSpawner(SandboxConfig{
			AuthDir:       authDirWithToken(t),
			AuthEnv:       "CODEX_HOME",
			AuthSeedFiles: []string{bad},
		})
		home, owned, err := sp.episodeAuthHome(workdir(t))
		if err == nil {
			_ = os.RemoveAll(home)
			t.Fatalf("seed %q was accepted", bad)
		}
		if owned || home != "" {
			t.Fatalf("a refused seed must leave no credential home, got %q owned=%v", home, owned)
		}
	}
}
