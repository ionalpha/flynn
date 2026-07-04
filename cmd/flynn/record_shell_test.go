package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/session"
)

// recordState reads the host's projected record state under the lock the host mutates it
// with, so the assertion does not race the turn goroutine that folded it.
func recordState(h *sessionHost) session.RecordState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.proj.Record
}

// TestShellSealVerifyMovesBadge drives a turn, then /seal and /verify through the shell,
// and proves the status badge advances recording -> sealed -> verified as each command
// completes. It is the end-to-end proof that the in-session record commands both act on
// the run and reflect their outcome in the badge.
func TestShellSealVerifyMovesBadge(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "done"})
	host.s.signer = selfCertifyingSigner(t)

	// A turn so the run exists and has events to seal.
	host.submit("hello", nil)
	waitIdle(t, host)
	if got := recordState(host); got != session.RecordRecording {
		t.Fatalf("record = %q before sealing, want recording", got)
	}

	host.submit("/seal", nil)
	waitIdle(t, host)
	if got := recordState(host); got != session.RecordSealed {
		t.Fatalf("record = %q after /seal, want sealed", got)
	}

	host.submit("/verify", nil)
	waitIdle(t, host)
	if got := recordState(host); got != session.RecordVerified {
		t.Fatalf("record = %q after /verify, want verified", got)
	}

	// The verify report reached the scrollback, so the user sees the tiers, not just the
	// badge.
	if !strings.Contains(ui.transcript(), "integrity:") {
		t.Error("verify report did not reach the scrollback")
	}
}

// TestShellSealWithoutKeyReports proves /seal on a session with no signing key reports
// the run cannot be sealed and leaves the badge unchanged, rather than failing silently.
func TestShellSealWithoutKeyReports(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "done"})
	host.s.signer = nil

	host.submit("hello", nil)
	waitIdle(t, host)
	host.submit("/seal", nil)
	waitIdle(t, host)

	if got := recordState(host); got != session.RecordRecording {
		t.Fatalf("record = %q, want recording (seal without a key must not advance it)", got)
	}
	if !strings.Contains(ui.transcript(), "no instance signing key") {
		t.Error("seal without a key did not report why")
	}
}
