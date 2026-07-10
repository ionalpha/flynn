//go:build !linux

package sandbox

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/netguard"
)

// On a platform whose children share the host's network stack, one proxy on the host's
// loopback serves every child and is started on first use. Linux gives each child its own
// namespace and so its own proxy; that path is proven in egress_linux_test.go.
func TestEnsureProxyStartsOnceOnLoopback(t *testing.T) {
	e := &egressConfig{policy: netguard.PublicOnly()}
	defer e.close()
	addr, err := e.ensureProxy()
	if err != nil {
		t.Fatalf("ensureProxy: %v", err)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("proxy bound %q, want loopback", addr)
	}
	// Idempotent: a second call returns the same running proxy.
	addr2, err := e.ensureProxy()
	if err != nil || addr2 != addr {
		t.Errorf("ensureProxy not idempotent: %q/%v vs %q", addr2, err, addr)
	}
}

// On a platform with no enforcement leg, a governed-egress launch must refuse rather than
// run the child with its direct egress open (refuse-rather-than-weaken).
