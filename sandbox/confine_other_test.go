//go:build !linux

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestNetworkDenyFailsLoudWhereUnsupported proves the safety default on a platform
// without kernel network isolation yet: a command asked to run with no network must
// fail, never run with the network silently still open.
func TestNetworkDenyFailsLoudWhereUnsupported(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithNetworkDenied())
	if err != nil {
		t.Fatal(err)
	}
	_, err = sb.Exec(context.Background(), Command{Line: "echo should-not-run"})
	if err == nil {
		t.Fatal("a network-denied command on an unsupported platform must fail, not run with the network open")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("want an unsupported-platform error, got %v", err)
	}
}
