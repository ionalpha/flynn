package e2e

import (
	"strings"
	"testing"
)

// TestBindGuardRefusesWildcard asserts the inbound bind guard refuses a wildcard bind
// (0.0.0.0, every interface) unconditionally: exposing the control-plane API on every
// interface is never allowed by default, so a run that asks for it is refused with a
// distinct error and a non-zero exit rather than silently listening to the world.
func TestBindGuardRefusesWildcard(t *testing.T) {
	in := newInstance(t)
	res := in.run("serve", "--api-addr", "0.0.0.0:8080")
	requireExit(t, res, 1, "serve on wildcard")
	low := strings.ToLower(res.combined())
	requireContains(t, low, "bind_denied", "wildcard refusal is a bind-guard denial")
	requireContains(t, low, "wildcard", "refusal names the wildcard address")
}

// TestBindGuardRefusesNonLoopback asserts a non-loopback bind is refused without an
// explicit exposure opt-in: the default posture is loopback-only, so binding a routable
// address needs a deliberate opt-in, not a silent default.
func TestBindGuardRefusesNonLoopback(t *testing.T) {
	in := newInstance(t)
	res := in.run("serve", "--api-addr", "8.8.8.8:8080")
	requireExit(t, res, 1, "serve on non-loopback")
	low := strings.ToLower(res.combined())
	requireContains(t, low, "bind_denied", "non-loopback refusal is a bind-guard denial")
	requireContains(t, low, "non-loopback", "refusal names the non-loopback address")
}

// TestServeGeneratesOperatorToken asserts the control-plane API fails closed: with no
// operator token supplied, serve generates one and shows it once rather than starting
// open. The refused wildcard bind is incidental here; the token line must appear before
// the bind is attempted, proving access is never dropped to open.
func TestServeGeneratesOperatorToken(t *testing.T) {
	in := newInstance(t)
	// Use a wildcard address so serve refuses and exits fast instead of blocking on a
	// live listener; the token is generated before the bind, so it is still printed.
	res := in.run("serve", "--api-addr", "0.0.0.0:8080")
	combined := res.combined()
	requireContains(t, combined, "FLYNN_API_TOKEN=", "an operator token is generated when none is given")
	requireContains(t, strings.ToLower(combined), "authorization: bearer", "the token is presented as a bearer credential")
}
