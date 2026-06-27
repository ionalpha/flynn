package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/netguard"
)

func TestProxyEnvVarsPointAtProxy(t *testing.T) {
	vars := proxyEnvVars("127.0.0.1:9999")
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if vars[k] != "http://127.0.0.1:9999" {
			t.Errorf("%s = %q, want http://127.0.0.1:9999", k, vars[k])
		}
	}
	// The child may still reach its own loopback without the proxy.
	if !strings.Contains(vars["NO_PROXY"], "127.0.0.1") {
		t.Errorf("NO_PROXY = %q, want it to exempt loopback", vars["NO_PROXY"])
	}
}

func TestMergeEnvOverlaysAndSorts(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, map[string]string{"B": "two", "C": "3"})
	want := []string{"A=1", "B=two", "C=3"}
	if len(got) != len(want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merged = %v, want %v", got, want)
		}
	}
}

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

// With no platform enforcement leg registered, a governed-egress launch must refuse
// rather than run the child with its direct egress open (refuse-rather-than-weaken).
func TestGovernedEgressRefusesWithoutPlatformLeg(t *testing.T) {
	if platformEgressConfiner != nil {
		t.Skip("a platform egress leg is registered; refusal path not applicable")
	}
	dir := t.TempDir()
	l, err := NewLocal(dir, WithEgress(netguard.PublicOnly()))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	defer func() { _ = l.Close() }()

	_, err = l.Exec(context.Background(), Command{Line: "echo hi"})
	if !errors.Is(err, errEgressUnsupported) {
		t.Fatalf("governed egress without a leg should refuse with errEgressUnsupported, got %v", err)
	}
}

func TestNoEgressConfigIsInert(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	defer func() { _ = l.Close() }()
	if l.egressActive() {
		t.Error("a sandbox without WithEgress must not govern egress")
	}
}
