package governed

import (
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// TestFailClosedGuard proves an unknown or empty guard name is not a guard: it cannot be
// waived, so a typo cannot silently record an opt-out that disables nothing.
func TestFailClosedGuard(t *testing.T) {
	for _, g := range []Guard{"", " ", "Sandbox", "sandbox ", "all", "none", "*"} {
		if g.valid() {
			t.Errorf("guard %q is valid", g)
		}
		c := &config{}
		Waive(g, "because")(c)
		if c.err == nil {
			t.Errorf("waiving %q was accepted", g)
		}
		if len(c.waiveList) != 0 {
			t.Errorf("waiving %q was recorded", g)
		}
	}
	for _, g := range allGuards {
		for _, reason := range []string{"", " ", "\t\n"} {
			c := &config{}
			Waive(g, reason)(c)
			if c.err == nil || len(c.waiveList) != 0 {
				t.Errorf("waiving %s with reason %q was accepted", g, reason)
			}
		}
	}
}

// TestFailClosedShellPolicy proves the zero ShellPolicy is the strictest: no network and
// no readable credentials on any platform.
func TestFailClosedShellPolicy(t *testing.T) {
	var zero ShellPolicy
	if open, reason := zero.NetworkOpen(); open || reason != "" {
		t.Errorf("the zero policy opens the network: %v %q", open, reason)
	}
	if readable, reason := zero.CredentialsReadable(); readable || reason != "" {
		t.Errorf("the zero policy leaves credentials readable: %v %q", readable, reason)
	}
	// The accommodation flags only matter when the platform cannot enforce; without the
	// matching Allow flag they must change nothing.
	if open, _ := (ShellPolicy{AllowCredentialReads: false, Limits: sandbox.ResourceLimits{}}).NetworkOpen(); open {
		t.Error("an unrelated field opened the network")
	}
}
