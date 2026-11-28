package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

func TestGrantStatusText(t *testing.T) {
	tests := []struct {
		name string
		p    sandbox.GrantProgress
		want string
	}{
		{"start", sandbox.GrantProgress{Dir: `C:\repo`}, `granting sandbox access (Esc cancels): C:\repo`},
		{"waited", sandbox.GrantProgress{Dir: `C:\repo`, Elapsed: 65200 * time.Millisecond}, `granting sandbox access (1m5s, Esc cancels): C:\repo`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantStatusText(tc.p, "Esc cancels"); got != tc.want {
				t.Fatalf("grantStatusText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnGrantTakesOverTheLiveLineUntilDone shows the wait on the live line and
// puts the line the turn had back when the grant ends.
func TestOnGrantTakesOverTheLiveLineUntilDone(t *testing.T) {
	host, _ := newSlashHost(t, nil)
	status := host.onGrant("thinking...")
	live := func() string { return strings.Join(host.live.Render(80), "\n") }

	status(sandbox.GrantProgress{Dir: "repo"})
	if got := live(); !strings.Contains(got, "granting sandbox access (Esc cancels): repo") {
		t.Fatalf("live line during a grant = %q", got)
	}
	status(sandbox.GrantProgress{Dir: "repo", Done: true})
	if got := live(); !strings.Contains(got, "thinking...") || strings.Contains(got, "granting") {
		t.Fatalf("live line after the grant = %q", got)
	}
}

// TestLineGrantStatusAnnouncesOncePerGrant keeps a minutes-long grant from
// printing a line a second, while the next grant is still announced.
func TestLineGrantStatusAnnouncesOncePerGrant(t *testing.T) {
	var out bytes.Buffer
	s := &replSession{out: &out}
	status := s.lineGrantStatus()
	announced := func() int { return strings.Count(out.String(), "granting sandbox access (Ctrl-C cancels): repo") }
	report := sandbox.GrantProgress{Dir: "repo"}

	status(report)
	status(report)
	status(sandbox.GrantProgress{Dir: "repo", Done: true})
	if got := announced(); got != 1 {
		t.Fatalf("one grant was announced %d times:\n%s", got, out.String())
	}
	status(report)
	if got := announced(); got != 2 {
		t.Fatalf("a later grant was not announced:\n%s", out.String())
	}
}
