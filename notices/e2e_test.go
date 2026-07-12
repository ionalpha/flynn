package notices_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/notices"
)

// TestEndToEndAdvisoryReachesAnInstalledFlynn drives the whole channel the way a real
// installation runs it: a publisher signs a feed, an https origin serves it, a client with
// nothing on disk fetches it, and the next run prints the advisory to the user.
//
// This is the test the whole task exists for, so it is written to fail if any link in that
// chain quietly stops working, not just if a function returns the wrong value.
func TestEndToEndAdvisoryReachesAnInstalledFlynn(t *testing.T) {
	signer, ring := testKey(t)
	now := clock.NewManual(at("2026-07-02T00:00:00Z"))

	doc, err := signer.Sign(notices.Feed{
		Version: 1,
		Issued:  at("2026-07-01T00:00:00Z"),
		Expires: at("2026-07-08T00:00:00Z"),
		Notices: []notices.Notice{{
			ID:           "FLYNN-2026-0001",
			Severity:     notices.Security,
			Summary:      "a crafted goal could escape the sandbox on Linux",
			Detail:       "Upgrade to 0.1.4. There is no workaround.",
			URL:          "https://flynnhq.com/advisories/FLYNN-2026-0001",
			AffectedFrom: "0.1.0",
			FixedIn:      "0.1.4",
		}},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// What the origin serves is swapped later in the test (it turns hostile), and the
	// handler runs on the server's goroutine, so the document is held atomically rather
	// than in a plain variable two goroutines would be touching.
	var served atomic.Pointer[[]byte]
	served.Store(&doc)

	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(*served.Load())
	}))
	defer srv.Close()

	dir := t.TempDir()
	newClient := func(flynnVersion string) *notices.Client {
		return &notices.Client{
			Source:  notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()},
			Ring:    ring,
			Store:   notices.NewStore(dir),
			Clock:   now,
			Version: flynnVersion,
		}
	}

	// Run one, on a vulnerable version, with an empty data directory. There is nothing
	// cached, so it says nothing, and it fetches.
	first := newClient("0.1.2")
	if first.Show(&strings.Builder{}) {
		t.Fatal("a client with no cached feed printed something")
	}
	if err := first.RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected one fetch, got %d", hits.Load())
	}

	// Run two. The advisory reaches the user, unprompted, because they are on an affected
	// version. This is the entire point of the channel.
	var out strings.Builder
	if !newClient("0.1.2").Show(&out) {
		t.Fatal("the advisory did not reach a vulnerable installation")
	}
	got := out.String()
	for _, want := range []string{"SECURITY:", "escape the sandbox", "advisories/FLYNN-2026-0001"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the advisory was missing %q:\n%s", want, got)
		}
	}

	// Run three, same version, same day: it is still shown. A security notice does not go
	// quiet just because it has been seen once, and it does not refetch inside the
	// interval either.
	out.Reset()
	third := newClient("0.1.2")
	if !third.Show(&out) {
		t.Fatal("the advisory stopped being shown to a still-vulnerable installation")
	}
	if err := third.RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("third check: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("the client refetched inside the refresh interval: %d fetches", hits.Load())
	}

	// The user upgrades. The advisory stops applying, and nothing is printed. Silence here
	// is earned, not assumed.
	out.Reset()
	if newClient("0.1.4").Show(&out) {
		t.Fatalf("a fixed version was still shown the advisory:\n%s", out.String())
	}

	// A day passes: the client is due again and re-checks.
	now.Advance(notices.RefreshInterval + time.Minute)
	if err := newClient("0.1.4").RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("fourth check: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("the client did not re-check after the interval elapsed: %d fetches", hits.Load())
	}

	// Now the origin turns hostile and replays an older, genuinely signed feed that drops
	// the advisory. The client must refuse it and keep what it already trusted.
	stale, err := signer.Sign(notices.Feed{
		Version: 0,
		Issued:  at("2026-06-01T00:00:00Z"),
		Expires: at("2026-06-08T00:00:00Z"),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	served.Store(&stale)
	now.Advance(notices.RefreshInterval + time.Minute)

	rolled := newClient("0.1.2")
	if err := rolled.RefreshIfDue(context.Background()); err == nil {
		t.Fatal("a rolled-back feed was accepted from the origin")
	}
	out.Reset()
	if !rolled.Show(&out) || !strings.Contains(out.String(), "SECURITY:") {
		t.Fatalf("the rollback attempt suppressed the advisory the client had already trusted:\n%s", out.String())
	}
}

// TestEndToEndFloorFromTheFeedTightensARealRuntimeGate drives the payoff of the whole
// channel: a model-parser vulnerability is disclosed after this binary was built, the floor
// it needs is published in a signed feed, and an installed Flynn that never upgraded now
// refuses to let the vulnerable runtime parse a model file.
//
// The compiled-in llama.cpp floor is b8146. Before the feed, a b8200 runtime runs. After it,
// it does not.
func TestEndToEndFloorFromTheFeedTightensARealRuntimeGate(t *testing.T) {
	signer, ring := testKey(t)

	if err := inference.SafeToRun("llama.cpp", inference.Version{8200}); err != nil {
		t.Fatalf("precondition: b8200 should pass the built-in floor, got %v", err)
	}

	doc, err := signer.Sign(notices.Feed{
		Version: 1,
		Issued:  at("2026-07-01T00:00:00Z"),
		Expires: at("2026-07-08T00:00:00Z"),
		Notices: []notices.Notice{{
			ID:       "FLYNN-RT-2026-0009",
			Severity: notices.Security,
			Summary:  "llama.cpp before b8300 has a heap overflow in the GGUF parser; a malicious model runs code",
		}},
		Floors: []notices.Floor{{
			Runtime:    "llama.cpp",
			MinVersion: "8300",
			AdvisoryID: "FLYNN-RT-2026-0009",
		}},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	c := &notices.Client{
		Source:  notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()},
		Ring:    ring,
		Store:   notices.NewStore(t.TempDir()),
		Clock:   clock.NewManual(at("2026-07-02T00:00:00Z")),
		Version: "0.1.2",
	}
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// This is the line the binary runs at startup (cmd/flynn applyRuntimeFloors).
	for _, f := range c.Floors() {
		inference.Raise(f.Runtime, inference.ParseVersion(f.MinVersion), f.AdvisoryID)
	}

	err = inference.SafeToRun("llama.cpp", inference.Version{8200})
	if err == nil {
		t.Fatal("a runtime below the floor the feed published was still allowed to parse a model file")
	}
	if !strings.Contains(err.Error(), "FLYNN-RT-2026-0009") {
		t.Fatalf("the refusal did not name the advisory: %v", err)
	}

	// The gate tightened, and nothing loosened: a runtime that was refused before is still
	// refused, and the newly required version passes.
	if err := inference.SafeToRun("llama.cpp", inference.Version{1}); err == nil {
		t.Fatal("an ancient runtime became runnable")
	}
	if err := inference.SafeToRun("llama.cpp", inference.Version{8300}); err != nil {
		t.Fatalf("the version the feed requires was itself refused: %v", err)
	}
}

// TestBackgroundRefreshDoesNotBlockTheCommand: the fetch must never be on the path of the
// work the user asked for. An origin that hangs must cost the command nothing.
func TestBackgroundRefreshDoesNotBlockTheCommand(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release // an origin that never answers
	}))
	defer srv.Close()
	defer close(release)

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 9
	}
	ring := notices.NewKeyring()
	if err := ring.Add("k", ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("keyring: %v", err)
	}

	c := &notices.Client{
		Source:  notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()},
		Ring:    ring,
		Store:   notices.NewStore(t.TempDir()),
		Clock:   clock.System{},
		Version: "0.1.2",
	}

	done := make(chan struct{})
	go func() {
		c.Background(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Background blocked the caller on a hung origin")
	}
}
