package e2e

import (
	"strings"
	"testing"
)

// TestEgressDeniesPrivateAndMetadata asserts the default-deny outbound posture through
// the binary: an attempt to reach a private, link-local, or cloud-metadata address is
// refused by the egress guard, not dialed. The model endpoint is the reachable outbound
// path a run exercises on every turn, so pointing it at each forbidden address and
// requiring an egress_denied refusal proves the SSRF-class targets are blocked at the
// dial, with a distinct non-zero exit.
func TestEgressDeniesPrivateAndMetadata(t *testing.T) {
	// Each target is https (so the base-URL safety check admits it) but non-public, so
	// the egress policy must refuse the dial. 169.254.169.254 is the cloud-metadata
	// endpoint; the others are private and link-local ranges.
	targets := []string{
		"https://169.254.169.254", // cloud metadata
		"https://10.0.0.1",        // private
		"https://192.168.1.1",     // private
		"https://172.16.0.1",      // private
	}
	for _, target := range targets {
		t.Run(strings.TrimPrefix(target, "https://"), func(t *testing.T) {
			in := newInstance(t)
			in.setEnv("OPENAI_API_KEY", "sk-e2e")
			in.setEnv("OPENAI_BASE_URL", target)

			res := in.run("-no-learn", "goal", "reach a forbidden address")
			requireExit(t, res, 1, "goal against a forbidden egress target")
			requireContains(t, res.combined(), "egress_denied", "egress guard refused the dial")
		})
	}
}
