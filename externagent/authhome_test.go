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
	home, seeded, err := sp.episodeAuthHome(wd)
	if err != nil {
		t.Fatalf("episodeAuthHome: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	if len(seeded) == 0 {
		t.Fatalf("a seeded credential home must report the copies it made, so they are deleted with the episode")
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

// TestEpisodeAuthHomeGathersSeedPaths proves the multi-source seed gathers files that live
// in different directories into one credential home by base name, so a CLI whose config and
// token are split (Claude Code) is given one directory it can be pointed at. The home is
// owned (writable and deleted with the episode), sits outside the recorded workspace, and a
// source that does not exist is skipped rather than fabricated.
func TestEpisodeAuthHomeGathersSeedPaths(t *testing.T) {
	src := t.TempDir()
	cfg := filepath.Join(src, ".claude.json")
	sub := filepath.Join(src, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(sub, ".credentials.json")
	if err := os.WriteFile(cfg, []byte(`{"config":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	wd := workdir(t)
	sp := NewSandboxSpawner(SandboxConfig{
		AuthEnv:       "CLAUDE_CONFIG_DIR",
		AuthSeedPaths: []string{cfg, creds, filepath.Join(src, "absent.json")},
	})
	home, seeded, err := sp.episodeAuthHome(wd)
	if err != nil {
		t.Fatalf("episodeAuthHome: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	if len(seeded) == 0 {
		t.Fatalf("an assembled credential home must report the copies it made, so they are deleted with the episode")
	}
	// Both files, from two different source directories, land flat by base name.
	if b, err := os.ReadFile(filepath.Join(home, ".credentials.json")); err != nil || !strings.Contains(string(b), "secret") {
		t.Fatalf("the token was not gathered into the home: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatalf("the config was not gathered into the home: %v", err)
	}
	// The home is outside the recorded workspace.
	if rel, err := filepath.Rel(wd, home); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the credential home %s is inside the recorded workspace %s", home, wd)
	}
	// A missing source is skipped, not fabricated.
	if _, err := os.Stat(filepath.Join(home, "absent.json")); err == nil {
		t.Fatalf("a missing seed source was created")
	}
	// The seed paths take precedence and the home is writable only because it is owned.
	got := writableDirsOf(t, sp.episodeOptions(home, true))
	if len(got) != 1 || !sameDir(t, got[0], home) {
		t.Fatalf("the assembled home must be granted write as an owned copy, got %q", got)
	}
}

// TestEpisodeAuthHomeWithoutSeedFilesUsesAuthDir proves the read-only behaviour is intact
// where no seeding is configured (a detection probe), and that the spawner claims no
// ownership of the host's directory: a stray delete of a real credential home is the worst
// thing this code could do.
func TestEpisodeAuthHomeWithoutSeedFilesUsesAuthDir(t *testing.T) {
	auth := authDirWithToken(t)
	sp := NewSandboxSpawner(SandboxConfig{AuthDir: auth, AuthEnv: "CODEX_HOME"})
	home, seeded, err := sp.episodeAuthHome(workdir(t))
	if err != nil {
		t.Fatalf("episodeAuthHome: %v", err)
	}
	if home != auth || len(seeded) != 0 {
		t.Fatalf("expected the unowned host auth dir with nothing copied, got home=%q seeded=%v", home, seeded)
	}
}

// TestEpisodeAuthHomeNoAuthDir proves a host with no resolvable credential home yields no
// home and no seeding, rather than an empty directory the CLI would read as logged out
// while the spawner reports it seeded one.
func TestEpisodeAuthHomeNoAuthDir(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{AuthEnv: "CODEX_HOME", AuthSeedFiles: []string{"auth.json"}})
	home, seeded, err := sp.episodeAuthHome(workdir(t))
	if err != nil || home != "" || len(seeded) != 0 {
		t.Fatalf("expected no credential home, got home=%q seeded=%v err=%v", home, seeded, err)
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
		home, seeded, err := sp.episodeAuthHome(workdir(t))
		if err == nil {
			_ = sp.Close()
			t.Fatalf("seed %q was accepted", bad)
		}
		if len(seeded) != 0 || home != "" {
			t.Fatalf("a refused seed must leave no credential home, got %q seeded=%v", home, seeded)
		}
	}
}

// TestRunHomeOutlivesAnEpisodeButCredentialsDoNot is the property a multi-turn session
// rests on, and the one it must not buy at the cost of the credential's lifetime.
//
// The home is the run's: it is where the CLI keeps its own conversation, and a later turn
// asks the CLI to continue the conversation it opened. A home thrown away with each
// episode would take that conversation with it, and the CLI would be told to resume
// something that no longer exists. So every episode of a run gets the same directory.
//
// The credentials in it are the episode's: they are copied in for the launch and deleted
// when it ends, so a copied token still never outlives the episode it was copied for. What
// persists between turns is the CLI's own state, not the key.
func TestRunHomeOutlivesAnEpisodeButCredentialsDoNot(t *testing.T) {
	auth := authDirWithToken(t)
	wd := workdir(t)
	sp := NewSandboxSpawner(SandboxConfig{
		AuthDir:       auth,
		AuthEnv:       "CODEX_HOME",
		AuthSeedFiles: []string{"auth.json"},
	})
	t.Cleanup(func() { _ = sp.Close() })

	first, seeded, err := sp.episodeAuthHome(wd)
	if err != nil {
		t.Fatalf("episodeAuthHome: %v", err)
	}
	// The CLI writes its conversation into the home while the episode runs.
	convo := filepath.Join(first, "conversation.jsonl")
	if err := os.WriteFile(convo, []byte(`{"turn":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The episode ends: its credentials go, the home stays.
	removeSeeds(seeded)
	if _, err := os.Stat(filepath.Join(first, "auth.json")); err == nil {
		t.Fatal("the copied token outlived the episode it was copied for")
	}
	if _, err := os.Stat(convo); err != nil {
		t.Fatalf("the CLI's conversation did not survive the episode: %v", err)
	}

	// The next turn of the same run lands in the same home, so the CLI finds the
	// conversation it is being asked to continue, and gets a fresh copy of the credential.
	second, seeded2, err := sp.episodeAuthHome(wd)
	if err != nil {
		t.Fatalf("episodeAuthHome (second turn): %v", err)
	}
	if second != first {
		t.Fatalf("a later turn got a different home (%s, was %s): the CLI would have no conversation to resume", second, first)
	}
	if _, err := os.Stat(convo); err != nil {
		t.Fatalf("the conversation is missing from the second turn's home: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(second, "auth.json")); err != nil || !strings.Contains(string(b), "secret") {
		t.Fatalf("the second turn was not given the credential it needs: %q err=%v", b, err)
	}
	removeSeeds(seeded2)

	// The run ends: the home goes, and with it everything the CLI wrote.
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(first); err == nil {
		t.Fatal("the harness home outlived the run")
	}
}
