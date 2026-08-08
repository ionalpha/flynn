package notices_test

// Whether a notice reaches this build: the version constraint, and whether the feed has
// gone stale. Both fail open in the same direction. An unparseable bound reads as "no
// constraint" rather than "no match", and a feed with no expiry goes stale relative to
// when it was issued, so neither a typo nor an omission can hide an advisory.

import (
	"testing"
	"time"

	"github.com/ionalpha/flynn/notices"
)

// TestStaleFallsBackToIssuedWhenExpiryIsAbsent pins that a publisher cannot make a feed
// immortal by omitting the expiry: with no expiry the feed goes stale StaleAfter from
// when it was issued.
func TestStaleFallsBackToIssuedWhenExpiryIsAbsent(t *testing.T) {
	issued := at("2026-07-01T00:00:00Z")
	noExpiry := notices.Feed{Version: 1, Issued: issued}

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"just after issue", issued.Add(time.Hour), false},
		{"one hour before the window closes", issued.Add(notices.StaleAfter - time.Hour), false},
		{"past the window", issued.Add(notices.StaleAfter + time.Second), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := noExpiry.Stale(tc.now); got != tc.want {
				t.Fatalf("Stale(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}

	// An explicit expiry takes precedence over the issued-plus-window rule, in both
	// directions: a short expiry goes stale early.
	short := notices.Feed{Version: 1, Issued: issued, Expires: issued.Add(time.Hour)}
	if !short.Stale(issued.Add(2 * time.Hour)) {
		t.Fatal("a feed past its explicit expiry should be stale")
	}
	if short.Stale(issued.Add(30 * time.Minute)) {
		t.Fatal("a feed inside its explicit expiry should not be stale")
	}
}

// TestAppliesToleratesOddVersionStrings walks the version parser's refusals. Each
// unparseable bound reads as "no constraint" rather than "no match", so a typo in a
// published feed can never silently hide an advisory from everyone.
func TestAppliesToleratesOddVersionStrings(t *testing.T) {
	n := notices.Notice{
		ID: "N", Severity: notices.Security, Summary: "s",
		AffectedFrom: "0.1.0", FixedIn: "0.1.4",
	}
	tests := []struct {
		name    string
		running string
		want    bool
	}{
		{"leading v is tolerated", "v0.1.2", true},
		{"a pre-release suffix is ignored", "0.1.2-rc1", true},
		{"a build suffix is ignored", "0.1.2+deadbeef", true},
		{"a fixed release is not affected", "v0.1.4", false},
		{"a version below the range is not affected", "0.0.9", false},
		// A missing trailing component reads as zero, so 0.1 is 0.1.0, which is the
		// first affected release rather than one below it.
		{"a two-component version compares as if trailing zero", "0.1", true},
		{"an unstamped build hears everything", "", true},
		{"the development placeholder hears everything", "0.0.0-dev", true},
		{"a non-numeric version hears everything", "nightly", true},
		{"an empty component hears everything", "0..1", true},
		{"an absurd component hears everything", "99999999999.0.0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := notices.Applies(n, tc.running); got != tc.want {
				t.Fatalf("Applies(running=%q) = %v, want %v", tc.running, got, tc.want)
			}
		})
	}

	// A notice with an unparseable bound still reaches a normal running version,
	// because an unreadable bound is no bound at all.
	broken := notices.Notice{ID: "B", Severity: notices.Security, Summary: "s", FixedIn: "not-a-version"}
	if !notices.Applies(broken, "1.2.3") {
		t.Fatal("an unparseable FixedIn must not hide the notice")
	}
}
