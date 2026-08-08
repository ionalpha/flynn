package notices_test

// The client's own behaviour: when it refreshes, what it trusts, and what it leaves
// running. A rollback floor is honoured only from a feed that verified, the refresh
// interval is respected rather than checked on every command, the off switch stops both
// the refresh and the background work, and a background refresh outlives the command
// context that started it.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/notices"
)

// clientOn returns a Client wired to a test origin serving doc, with a manual clock.
func clientOn(t *testing.T, doc []byte, ring *notices.Keyring, dir string, now clock.Clock, version string) (*notices.Client, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)
	return &notices.Client{
		Source:  notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()},
		Ring:    ring,
		Store:   notices.NewStore(dir),
		Clock:   now,
		Version: version,
	}, &hits
}

// TestFloorsComeOnlyFromATrustedFeed proves the version gates a client applies are read
// from the signed document and from nowhere else: with no cached feed there are no
// floors, and with the channel switched off there are none either, so the off switch
// cannot be used as a way to keep gating a user who thinks they turned it all off.
func TestFloorsComeOnlyFromATrustedFeed(t *testing.T) {
	signer, ring := testKey(t)
	f := feed(1, advisory())
	f.Floors = []notices.Floor{{Runtime: "llama.cpp", MinVersion: "b4200", AdvisoryID: "FLYNN-2026-0001"}}
	doc, err := signer.Sign(f)
	if err != nil {
		t.Fatal(err)
	}
	now := clock.NewManual(at("2026-07-02T00:00:00Z"))
	dir := t.TempDir()
	c, _ := clientOn(t, doc, ring, dir, now, "0.1.2")

	// Nothing cached yet: no floors, and no network touched to find that out.
	if got := c.Floors(); len(got) != 0 {
		t.Fatalf("Floors before any refresh = %v, want none", got)
	}

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got := c.Floors()
	if len(got) != 1 || got[0].Runtime != "llama.cpp" || got[0].MinVersion != "b4200" {
		t.Fatalf("Floors after refresh = %+v, want the feed's floor", got)
	}

	// With the channel off, the floors are gone even though the cache still holds them.
	t.Setenv(notices.OffEnv, "1")
	if got := c.Floors(); len(got) != 0 {
		t.Fatalf("Floors with the channel off = %v, want none", got)
	}
}

// TestRefreshIfDueRespectsTheInterval pins the throttle: a client that just checked does
// not fetch again, so the origin does not become an activity log of every command a user
// runs. Advancing past the interval makes it due again.
func TestRefreshIfDueRespectsTheInterval(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	now := clock.NewManual(at("2026-07-02T00:00:00Z"))
	c, hits := clientOn(t, doc, ring, t.TempDir(), now, "0.1.2")
	ctx := context.Background()

	// A client that has never checked is due immediately.
	if err := c.RefreshIfDue(ctx); err != nil {
		t.Fatalf("first RefreshIfDue: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the first check should fetch once, saw %d", got)
	}

	// Immediately after, it is not due, and nothing goes out.
	if err := c.RefreshIfDue(ctx); err != nil {
		t.Fatalf("second RefreshIfDue: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("a check inside the interval still fetched, saw %d hits", got)
	}

	now.Advance(notices.RefreshInterval + time.Second)
	if err := c.RefreshIfDue(ctx); err != nil {
		t.Fatalf("third RefreshIfDue: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("a check past the interval should fetch, saw %d hits", got)
	}
}

// TestRefreshIfDueSurfacesACorruptTrustFile pins that the throttle does not swallow a
// state error: a client that cannot read its rollback mark reports it rather than
// fetching and re-accepting a feed it can no longer compare against.
func TestRefreshIfDueSurfacesACorruptTrustFile(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := clock.NewManual(at("2026-07-02T00:00:00Z"))
	c, hits := clientOn(t, doc, ring, dir, now, "0.1.2")

	if err := os.MkdirAll(filepath.Join(dir, "notices"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notices", "trust.json"), []byte("{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("a corrupt trust file should surface, not be silently overwritten")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("the origin was contacted despite unreadable trust state (%d hits)", got)
	}
}

// TestOffSwitchStopsRefreshAndBackground pins that the one switch really is one switch:
// with it set, neither the throttled refresh nor the background goroutine goes near the
// network.
func TestOffSwitchStopsRefreshAndBackground(t *testing.T) {
	t.Setenv(notices.OffEnv, "1")
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	now := clock.NewManual(at("2026-07-02T00:00:00Z"))
	c, hits := clientOn(t, doc, ring, t.TempDir(), now, "0.1.2")

	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("RefreshIfDue with the channel off: %v", err)
	}
	// Background returns immediately and, with the channel off, starts no goroutine at
	// all, so there is nothing to wait for.
	c.Background(context.Background())
	if got := hits.Load(); got != 0 {
		t.Fatalf("the off switch did not stop the fetch (%d hits)", got)
	}

	var buf bytes.Buffer
	if c.Show(&buf) {
		t.Fatalf("the off switch did not stop rendering: %q", buf.String())
	}
}

// TestBackgroundOutlivesTheCommandContext is the reason the background fetch detaches:
// cancelling the user's work must not be what decides whether a vulnerability is ever
// heard about. The fetch is started under an already-cancelled context and still
// completes.
func TestBackgroundOutlivesTheCommandContext(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := clock.NewManual(at("2026-07-02T00:00:00Z"))

	fetched := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
		select {
		case fetched <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	c := &notices.Client{
		Source:  notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()},
		Ring:    ring,
		Store:   notices.NewStore(dir),
		Clock:   now,
		Version: "0.1.2",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the user's command is already over
	c.Background(ctx)

	select {
	case <-fetched:
	case <-time.After(10 * time.Second):
		t.Fatal("the background refresh never reached the origin")
	}

	// The feed it fetched is what the next run shows, which is the whole point of doing
	// it in the background. The write lands just after the handler returns, so wait for
	// the cache rather than racing it.
	timeout := time.After(10 * time.Second)
	for {
		var buf bytes.Buffer
		if c.Show(&buf) && strings.Contains(buf.String(), "SECURITY:") {
			break
		}
		select {
		case <-timeout:
			t.Fatal("the background refresh never cached a feed the next run could show")
		case <-time.After(5 * time.Millisecond):
		}
	}
	waitForQuietStore(t, dir)
}

// waitForQuietStore blocks until the detached refresh has finished writing. A showable
// feed is not the end of its work: the refresh saves the feed and only then the trust
// state, each through a temporary file it renames into place. Returning at the first
// showable feed leaves the goroutine still writing into the directory the test framework
// is about to remove, which on Windows fails the removal outright ("directory is not
// empty") and elsewhere merely leaves the race unobserved. The last write is the trust
// rename, so the store is quiet once trust.json exists with no temporaries beside it.
func waitForQuietStore(t *testing.T, dir string) {
	t.Helper()
	store := filepath.Join(dir, "notices")
	deadline := time.After(10 * time.Second)
	for {
		if entries, err := os.ReadDir(store); err == nil {
			settled, temps := false, false
			for _, e := range entries {
				switch {
				case strings.Contains(e.Name(), ".tmp-"):
					temps = true
				case e.Name() == "trust.json":
					settled = true
				}
			}
			if settled && !temps {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("the background refresh never finished writing its trust state")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
