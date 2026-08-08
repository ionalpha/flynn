package notices_test

// Fetching the feed, and what the transport refuses to do. The client is deliberately
// narrow: no redirects, no private addresses, no scheme it did not expect. A fetch that
// fails must also leave a good cache alone, because a transient network failure is not
// evidence that the advisory it already holds is wrong.

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

func TestFetchRefusesUnsafeURLs(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"plain http", "http://flynnhq.com/notices.cose"},
		{"a file path", "file:///etc/notices.cose"},
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := notices.Source{URL: tc.url}.Fetch(context.Background())
			if err == nil {
				t.Fatal("expected the fetch to be refused")
			}
			if !strings.Contains(err.Error(), "https") {
				t.Fatalf("got %v, want an https requirement", err)
			}
		})
	}
}

func TestFetchRejectsANonOKStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := notices.Source{URL: srv.URL, HTTP: srv.Client()}.Fetch(context.Background())
	if err == nil {
		t.Fatal("a 404 should not produce a feed")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v, want the status in the error", err)
	}
}

func TestFetchReportsAnUnreachableOrigin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	client := srv.Client()
	srv.Close() // nothing is listening now

	_, err := notices.Source{URL: url, HTTP: client}.Fetch(context.Background())
	if err == nil {
		t.Fatal("a dead origin should be an error")
	}
}

// TestSafeClientRefusesRedirects pins the transport the production fetch uses. A static
// signed document has no reason to redirect, and following one is how a fetch ends up at
// an origin nobody audited.
func TestSafeClientRefusesRedirects(t *testing.T) {
	c := notices.SafeClient()
	if c.Timeout != notices.FetchTimeout {
		t.Fatalf("SafeClient timeout = %v, want %v", c.Timeout, notices.FetchTimeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("SafeClient must refuse redirects")
	}
	req, err := http.NewRequest(http.MethodGet, "https://flynnhq.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Fatal("SafeClient followed a redirect")
	}
}

// TestSafeClientWillNotReachAPrivateAddress proves the notice fetch is governed by the
// same egress rules as everything else in the process: it cannot be pointed at a private
// address, so a DNS answer that resolves the feed origin to localhost gets it nowhere.
func TestSafeClientWillNotReachAPrivateAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte{0xd2, 0x84})
	}))
	defer srv.Close()

	// srv.URL is a loopback address, which the public-only dialer must refuse. The
	// default client (nil HTTP) is the one under test here.
	_, err := notices.Source{URL: strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)}.
		Fetch(context.Background())
	if err == nil {
		t.Fatal("the safe client reached a private address")
	}
}

// TestRefreshDoesNotEvictAGoodCache is the failure-mode guarantee: a hostile or broken
// origin cannot even destroy the last feed we did trust. The worst it can do is fail.
func TestRefreshDoesNotEvictAGoodCache(t *testing.T) {
	signer, ring := testKey(t)
	good, err := signer.Sign(feed(2, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := notices.NewStore(dir)
	now := at("2026-07-02T00:00:00Z")

	// Establish a trusted cache.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(good)
	}))
	src := notices.Source{URL: srv.URL, HTTP: srv.Client()}
	if _, err := notices.Refresh(context.Background(), src, ring, store, now); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	srv.Close()

	// Now the origin turns hostile in every way it can: a rolled-back feed, a feed
	// signed by a key that is not in the ring but claims the id of one that is, and
	// outright garbage. None of them may disturb the cache.
	otherSeed := make([]byte, ed25519.SeedSize)
	for i := range otherSeed {
		otherSeed[i] = byte(200 - i)
	}
	otherSigner, err := notices.NewSigner("test-key-1", ed25519.NewKeyFromSeed(otherSeed))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := otherSigner.Sign(feed(3, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	rolledBack, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}

	hostile := map[string][]byte{
		"rolled back":   rolledBack,
		"forged":        forged,
		"not a feed":    []byte("<html>404</html>"),
		"empty payload": {},
	}
	for name, body := range hostile {
		t.Run(name, func(t *testing.T) {
			bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(body)
			}))
			defer bad.Close()

			_, err := notices.Refresh(context.Background(),
				notices.Source{URL: bad.URL, HTTP: bad.Client()}, ring, store, now)
			if err == nil {
				t.Fatal("a hostile origin was accepted")
			}

			// The good feed is exactly where it was.
			f, tr, ok := notices.Cached(store, ring)
			if !ok {
				t.Fatal("a hostile origin evicted the trusted cache")
			}
			if f.Version != 2 {
				t.Fatalf("cached feed is version %d, want the trusted 2", f.Version)
			}
			if tr.Version != 2 {
				t.Fatalf("trust mark is %d, want the trusted 2", tr.Version)
			}
		})
	}
}

// TestRefreshSurfacesAnUnreadableTrustFile pins that Refresh will not fetch-and-accept
// past a state file it could not read: without the rollback mark there is nothing to
// compare a feed's version against.
func TestRefreshSurfacesAnUnreadableTrustFile(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notices"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notices", "trust.json"), []byte("{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = notices.Refresh(context.Background(),
		notices.Source{URL: srv.URL, HTTP: srv.Client()},
		ring, notices.NewStore(dir), at("2026-07-02T00:00:00Z"))
	if err == nil {
		t.Fatal("Refresh should surface a trust file it cannot read")
	}
}

// TestRefreshReportsAFailureToCache pins that a feed which verified but could not be
// written is reported rather than returned as though it were cached. Silently returning
// it would mean the advisory is shown once and then, on the next run, is gone.
func TestRefreshReportsAFailureToCache(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	// A directory sitting where the cached feed file has to go: the fetch and the
	// verification both succeed, and only the write fails.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notices", "feed.cose"), 0o700); err != nil {
		t.Fatal(err)
	}

	f, err := notices.Refresh(context.Background(),
		notices.Source{URL: srv.URL, HTTP: srv.Client()},
		ring, notices.NewStore(dir), at("2026-07-02T00:00:00Z"))
	if err == nil {
		t.Fatal("a feed that could not be cached was reported as refreshed")
	}
	if f.Version != 0 {
		t.Fatalf("a failed refresh still returned a feed: %+v", f)
	}
}
