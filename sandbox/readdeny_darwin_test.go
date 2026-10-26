//go:build darwin

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatbeltHidesConfiguredPaths is the macOS read-deny guarantee, run against the real
// seatbelt: a command under the default confinement cannot read a hidden file, list a
// hidden directory or write into one, and it still reads a file beside them, so hiding a
// path takes away only that path.
func TestSeatbeltHidesConfiguredPaths(t *testing.T) {
	if !ReadDenySupported() {
		t.Fatal("macOS must report that it can hide a path")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const secret = "seatbelt-hidden-secret"
	dir := filepath.Join(base, "dotssh")
	file := filepath.Join(base, "credentials.json")
	beside := filepath.Join(base, "notes.txt")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// unlisted is named by no command below, so finding it in any output means the directory
	// was listed; an error message naming a path the command line gave it does not.
	const unlisted = "known_hosts_unlisted"
	for _, f := range []string{filepath.Join(dir, "id_ed25519"), filepath.Join(dir, unlisted), file} {
		if err := os.WriteFile(f, []byte(secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(beside, []byte("beside-ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := newTestLocal(t, WithDefaultConfinement(), WithUnreadablePaths(dir, file))
	run := func(line string) ExecResult {
		t.Helper()
		res, err := l.Exec(context.Background(), Command{Line: line})
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		return res
	}

	for _, line := range []string{
		"cat " + filepath.Join(dir, "id_ed25519"),
		"ls " + dir,
		"cat " + file,
	} {
		if res := run(line); res.ExitCode == 0 || strings.Contains(res.Output, secret) || strings.Contains(res.Output, unlisted) {
			t.Errorf("%q reached a hidden path: exit %d, output %q", line, res.ExitCode, res.Output)
		}
	}
	if res := run("echo x > " + filepath.Join(dir, "planted")); res.ExitCode == 0 {
		t.Errorf("a command wrote into a hidden directory: %q", res.Output)
	}
	if res := run("cat " + beside); res.ExitCode != 0 || !strings.Contains(res.Output, "beside-ok") {
		t.Errorf("hiding two paths took away a third: exit %d, output %q", res.ExitCode, res.Output)
	}
}
