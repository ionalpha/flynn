package sandbox

import (
	"context"
	"errors"
	"testing"
)

// TestReadDenyRefusesWhereUnsupported covers the platform that cannot hide a path
// (Windows): a complete configuration is still refused, since there is nothing to hide the
// path with.
func TestReadDenyRefusesWhereUnsupported(t *testing.T) {
	if ReadDenySupported() {
		t.Skip("this platform hides paths; the Linux and macOS read-deny tests cover it")
	}
	l := newTestLocal(t, WithDefaultConfinement(), WithUnreadablePaths(t.TempDir()))
	if _, err := l.Exec(context.Background(), Command{Line: "echo ran"}); !errors.Is(err, ErrReadDeny) {
		t.Fatalf("got %v, want ErrReadDeny", err)
	}
}

// TestReadDenyRefusesAnUnenforceableSet holds the rule that a read-deny set which cannot be
// applied stops the launch, on every platform: without a read-only host view the Local
// leaves the host's filesystem as it is, and a set that hides the working directory or a
// grant contradicts the Local's own configuration.
func TestReadDenyRefusesAnUnenforceableSet(t *testing.T) {
	secret := t.TempDir()
	work := t.TempDir()
	cases := map[string][]LocalOption{
		"no read-only host view":        {WithUnreadablePaths(secret)},
		"the working directory is held": {WithReadOnlyFS(), WithUnreadablePaths(work)},
		"a writable grant is held":      {WithReadOnlyFS(), WithWritableDir(secret), WithUnreadablePaths(secret)},
		"a readable grant is held":      {WithReadOnlyFS(), WithReadableDir(secret), WithUnreadablePaths(secret)},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			l, err := NewLocal(work, opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if _, err := l.Exec(context.Background(), Command{Line: "echo ran"}); !errors.Is(err, ErrReadDeny) {
				t.Fatalf("got %v, want ErrReadDeny", err)
			}
		})
	}
}

// TestUnreadablePathsAreRecorded holds the list a record of the run shows.
func TestUnreadablePathsAreRecorded(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	l := newTestLocal(t, WithUnreadablePaths(a, "", b))
	got := l.UnreadablePaths()
	if len(got) != 2 {
		t.Fatalf("got %v, want the two non-empty paths", got)
	}
	got[0] = "changed"
	if l.UnreadablePaths()[0] == "changed" {
		t.Fatal("UnreadablePaths returned the Local's own slice")
	}
}
