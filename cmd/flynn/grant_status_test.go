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
