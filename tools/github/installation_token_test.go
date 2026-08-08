package github_test

// Installation token lifecycle. The token is minted once and reused while it is valid,
// so a review does not mint one per API call, and it is refreshed before expiry rather
// than handed to a request that would outlive it. Which credential the reviewer uses in
// the first place is auth_test.go's subject.

import (
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/tools/github"
)

// The installation token is minted once and reused while valid, so a review does
// not mint a token per API call.
func TestInstallationTokenIsCached(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	for range 3 {
		if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
	if got := hub.tokensMinted.Load(); got != 1 {
		t.Fatalf("tokens minted = %d, want 1 (the cache is not working)", got)
	}
}

// A token near expiry is refreshed rather than handed to a request that outlives it.
func TestInstallationTokenRefreshesBeforeExpiry(t *testing.T) {
	hub := newFakeHub(t)
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	set := newSet(t, hub, func(c *github.Config) { c.Clock = clk })

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 1 {
		t.Fatalf("tokens minted = %d, want 1", got)
	}

	// The hub issues tokens expiring an hour from the test clock; advance past it.
	clk.Advance(2 * time.Hour)
	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 2 {
		t.Fatalf("tokens minted = %d, want 2 (an expired token was reused)", got)
	}
}
