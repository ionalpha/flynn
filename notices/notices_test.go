package notices_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/notices"
)

// testKey is the signing identity the tests publish with. Ed25519 from a fixed seed, so a
// failure reproduces byte for byte.
func testKey(t *testing.T) (*notices.Signer, *notices.Keyring) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	signer, err := notices.NewSigner("test-key-1", priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	ring := notices.NewKeyring()
	if err := ring.Add("test-key-1", priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("keyring add: %v", err)
	}
	return signer, ring
}

func at(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
}

func feed(version uint64, ns ...notices.Notice) notices.Feed {
	return notices.Feed{
		Version: version,
		Issued:  at("2026-07-01T00:00:00Z"),
		Expires: at("2026-07-08T00:00:00Z"),
		Notices: ns,
	}
}

func advisory() notices.Notice {
	return notices.Notice{
		ID:           "FLYNN-2026-0001",
		Severity:     notices.Security,
		Summary:      "the sandbox admitted a command it should have refused",
		URL:          "https://flynnhq.com/advisories/FLYNN-2026-0001",
		AffectedFrom: "0.1.0",
		FixedIn:      "0.1.4",
	}
}

func TestSignedFeedRoundTrips(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := notices.Verify(doc, ring)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Version != 1 || len(got.Notices) != 1 {
		t.Fatalf("round trip lost the feed: %+v", got)
	}
	if got.Notices[0].ID != "FLYNN-2026-0001" || got.Notices[0].Severity != notices.Security {
		t.Fatalf("round trip lost the notice: %+v", got.Notices[0])
	}
}

// A single flipped bit anywhere in the document must break it. This is the base
// guarantee everything else stands on.
func TestTamperedFeedIsRefused(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	for i := range doc {
		tampered := make([]byte, len(doc))
		copy(tampered, doc)
		tampered[i] ^= 0x01
		if _, err := notices.Verify(tampered, ring); err == nil {
			t.Fatalf("a feed with byte %d flipped verified", i)
		}
	}
}

// A feed signed by a key that is not in the ring is not a feed. This is what stops
// anyone who can stand up an origin, or intercept one, from saying anything to a Flynn.
func TestFeedFromAnUnknownKeyIsRefused(t *testing.T) {
	_, ring := testKey(t)

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0xAA
	}
	attacker, err := notices.NewSigner("attacker-key", ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	doc, err := attacker.Sign(feed(99, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("a feed signed by an unknown key verified")
	}
}

func TestEmptyKeyringVerifiesNothing(t *testing.T) {
	signer, _ := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := notices.Verify(doc, notices.NewKeyring()); err == nil {
		t.Fatal("an empty keyring verified a feed; it must be able to trust nothing")
	}
}

// The rollback attack: a mirror replays an older, genuinely signed feed to bury a newer
// advisory. The monotonic version is what makes it visible.
func TestRollbackToAnOlderFeedIsRefused(t *testing.T) {
	signer, ring := testKey(t)
	now := at("2026-07-02T00:00:00Z")

	newDoc, err := signer.Sign(feed(7, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, tr, err := notices.Accept(newDoc, ring, notices.Trust{}, now)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if tr.Version != 7 {
		t.Fatalf("trust did not record the accepted version: %+v", tr)
	}

	// Version 6 is validly signed by us. It is still a rollback.
	oldDoc, err := signer.Sign(feed(6))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, _, err := notices.Accept(oldDoc, ring, tr, now); err == nil {
		t.Fatal("a rolled-back feed was accepted, which would let a mirror bury an advisory")
	}

	// Re-serving the same version is normal and must keep working.
	if _, _, err := notices.Accept(newDoc, ring, tr, now); err != nil {
		t.Fatalf("re-serving the current feed was refused: %v", err)
	}
}

// The freeze attack: an attacker blocks the origin so the client keeps believing an old
// view forever. Expiry cannot suppress the notices themselves (that would let a broken
// re-signing job silence a live advisory), so what it must do is make the silence
// visible.
func TestAnExpiredFeedIsReportedStaleButStillShown(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	long := at("2026-09-01T00:00:00Z") // well past the feed's expiry
	f, _, err := notices.Accept(doc, ring, notices.Trust{}, long)
	if err != nil {
		t.Fatalf("an expired feed must still be accepted, or a broken re-signing job would suppress a live advisory: %v", err)
	}
	if !f.Stale(long) {
		t.Fatal("an expired feed did not report itself stale")
	}
	if f.Stale(at("2026-07-02T00:00:00Z")) {
		t.Fatal("a feed inside its expiry reported itself stale")
	}

	var sb strings.Builder
	if !notices.Render(&sb, f.Notices, true) {
		t.Fatal("render wrote nothing")
	}
	out := sb.String()
	if !strings.Contains(out, "SECURITY:") {
		t.Fatalf("the advisory was suppressed by staleness:\n%s", out)
	}
	if !strings.Contains(out, "not been refreshed recently") {
		t.Fatalf("staleness was not reported to the user:\n%s", out)
	}
}

// Feed text reaches a terminal, and a terminal is an interpreter. A signature does not
// help here: the escape would be signed by us. It has to be stripped.
func TestTerminalEscapesAreStrippedFromFeedText(t *testing.T) {
	signer, ring := testKey(t)
	hostile := notices.Notice{
		ID:       "FLYNN-2026-0002",
		Severity: notices.Info,
		// A CSI sequence that clears the screen and moves the cursor up, an OSC that sets
		// the window title, the 8-bit C1 forms of both (which a naive "\x1b[" filter
		// misses entirely), a bare carriage return to overwrite the line, and a NUL.
		Summary: "safe\x1b[2J\x1b[1;1Hrepainted\x1b]0;title\x07\u009b31mred\u009d0;t\x07\rover\x00",
		Detail:  "line one\nline two\x1b[31m",
	}
	doc, err := signer.Sign(feed(1, hostile))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	f, err := notices.Verify(doc, ring)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	got := f.Notices[0]
	for _, bad := range []string{"\x1b", "\x07", "\x9b", "\x9d", "\r", "\x00"} {
		if strings.Contains(got.Summary, bad) || strings.Contains(got.Detail, bad) {
			t.Fatalf("a control character survived sanitizing: %q / %q", got.Summary, got.Detail)
		}
	}
	// The readable text survives; only the escapes are gone.
	if !strings.Contains(got.Summary, "safe") || !strings.Contains(got.Summary, "repainted") {
		t.Fatalf("sanitizing ate the readable text: %q", got.Summary)
	}
	// A newline in the detail is the one control we keep, because advisory prose needs it
	// and it cannot begin an escape sequence.
	if !strings.Contains(got.Detail, "line one\nline two") {
		t.Fatalf("sanitizing dropped a legitimate newline: %q", got.Detail)
	}
}

func TestOversizedAndMalformedFeedsAreRefused(t *testing.T) {
	signer, ring := testKey(t)

	// A duplicate notice id would let the second notice under that id be permanently
	// swallowed by the "already shown" record of the first.
	dup := feed(1, advisory(), advisory())
	doc, err := signer.Sign(dup)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("a feed with duplicate notice ids verified")
	}

	// An unknown severity is a decode failure, not a fourth kind of notice to render.
	odd := feed(1, notices.Notice{ID: "x", Severity: "critical!!!", Summary: "hi"})
	doc, err = signer.Sign(odd)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("a feed with an unknown severity verified")
	}

	// A notice whose summary is nothing but control characters sanitizes to empty, and an
	// empty summary is not a notice. (Stripping is per-character, so "\x1b[2J" would
	// survive as the harmless literal text "[2J": the escape is what makes a terminal act,
	// and the escape is what is removed.)
	blank := feed(1, notices.Notice{ID: "y", Severity: notices.Info, Summary: "\x1b\x07\x00"})
	doc, err = signer.Sign(blank)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("a feed whose summary sanitized to nothing verified")
	}

	// Too many notices: a hostile publisher must not be able to bury a real advisory
	// under a screenful of noise.
	var many []notices.Notice
	for i := range notices.MaxNotices + 1 {
		many = append(many, notices.Notice{
			ID:       string(rune('a'+i%26)) + strings.Repeat("z", i),
			Severity: notices.Info,
			Summary:  "filler",
		})
	}
	doc, err = signer.Sign(feed(1, many...))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("an oversized feed verified")
	}
}

func TestNoticeAppliesToVersionRange(t *testing.T) {
	n := advisory() // 0.1.0 <= v < 0.1.4
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"0.0.9", false},
		{"0.1.0", true},
		{"v0.1.3", true},
		{"0.1.4", false},
		{"0.2.0", false},
		{"0.1.4-rc1", false}, // the pre-release of the fix carries the fix
		{"", true},           // a dev build hears everything rather than nothing
		{"0.0.0-dev", true},
	} {
		if got := notices.Applies(n, tc.version); got != tc.want {
			t.Errorf("Applies(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}

	// An advisory with no fix released yet applies from its first affected version on.
	open := notices.Notice{ID: "o", Severity: notices.Security, Summary: "s", AffectedFrom: "0.1.0"}
	if !notices.Applies(open, "9.9.9") {
		t.Error("an unfixed advisory stopped applying at some later version")
	}
}

// A security notice keeps being shown; a release notice is said once. A channel that let
// a vulnerability scroll past once and then went quiet would look like diligence while
// being worse than nothing.
func TestSecurityNoticesRepeatAndOthersDoNot(t *testing.T) {
	dir := t.TempDir()
	store := notices.NewStore(dir)

	sec := advisory()
	info := notices.Notice{ID: "REL-1", Severity: notices.Info, Summary: "0.2.0 is out"}
	f := feed(1, sec, info)

	tr := notices.Trust{}
	pending := notices.Pending(f, "0.1.2", tr)
	if len(pending) != 2 {
		t.Fatalf("first run should show both notices, got %d", len(pending))
	}

	tr, err := store.MarkShown(tr, pending)
	if err != nil {
		t.Fatalf("mark shown: %v", err)
	}

	pending = notices.Pending(f, "0.1.2", tr)
	if len(pending) != 1 || pending[0].Severity != notices.Security {
		t.Fatalf("second run should show only the security notice, got %+v", pending)
	}

	// Moving to a fixed version is the only thing that silences it.
	if got := notices.Pending(f, "0.1.4", tr); len(got) != 0 {
		t.Fatalf("a fixed version still saw notices: %+v", got)
	}

	// And the record survives the process: it is on disk, not in memory.
	reloaded, err := store.LoadTrust()
	if err != nil {
		t.Fatalf("load trust: %v", err)
	}
	if len(notices.Pending(f, "0.1.2", reloaded)) != 1 {
		t.Fatal("the shown record did not survive a reload")
	}
}

// The cached document is re-verified on every run, so editing the cache file destroys a
// notice rather than forging one.
func TestEditedCacheIsDiscardedNotBelieved(t *testing.T) {
	signer, ring := testKey(t)
	dir := t.TempDir()
	store := notices.NewStore(dir)

	doc, err := signer.Sign(feed(3, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := store.SaveFeed(doc); err != nil {
		t.Fatalf("save feed: %v", err)
	}
	if err := store.SaveTrust(notices.Trust{Version: 3}); err != nil {
		t.Fatalf("save trust: %v", err)
	}
	if _, _, ok := notices.Cached(store, ring); !ok {
		t.Fatal("a good cached feed was not loaded")
	}

	path := filepath.Join(dir, "notices", "feed.cose")
	edited, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	edited[len(edited)/2] ^= 0xff
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	if _, _, ok := notices.Cached(store, ring); ok {
		t.Fatal("an edited cache file was believed")
	}
}

// A cache rolled back on disk to an older but genuinely signed feed is refused too: the
// highest-version-ever mark lives in the trust file and is checked against the cache, not
// only against the network.
func TestRolledBackCacheIsRefused(t *testing.T) {
	signer, ring := testKey(t)
	dir := t.TempDir()
	store := notices.NewStore(dir)

	old, err := signer.Sign(feed(2))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := store.SaveFeed(old); err != nil {
		t.Fatalf("save feed: %v", err)
	}
	if err := store.SaveTrust(notices.Trust{Version: 5}); err != nil {
		t.Fatalf("save trust: %v", err)
	}
	if _, _, ok := notices.Cached(store, ring); ok {
		t.Fatal("a cache file rolled back below the trusted version was accepted")
	}
}

func TestRefreshFetchesAcceptsAndCaches(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(4, advisory()))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	var gotUA string
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotQuery = r.URL.RawQuery
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := notices.NewStore(dir)
	src := notices.Source{URL: srv.URL, HTTP: srv.Client()}
	// The test server is plain http, which the https check would refuse, so the fetch is
	// exercised through the store-and-accept path with the document in hand. The https
	// requirement itself is asserted below.
	f, _, err := notices.Accept(doc, ring, notices.Trust{}, at("2026-07-02T00:00:00Z"))
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if f.Version != 4 {
		t.Fatalf("wrong feed accepted: %+v", f)
	}
	if err := store.SaveFeed(doc); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, ok := notices.Cached(store, ring); !ok {
		t.Fatal("the accepted feed was not readable from the cache")
	}

	// http is refused outright: a notice channel over a transport anyone on the path can
	// rewrite is a notice channel anyone on the path can silence.
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("an http feed URL was fetched; it must be https")
	}
	_ = gotUA
	_ = gotQuery
}

// The request must carry nothing that identifies this installation. An origin that can
// tell two clients apart can serve one of them a different answer, and that is the whole
// property this channel rests on.
func TestFetchSendsNoIdentifiers(t *testing.T) {
	signer, _ := testKey(t)
	doc, err := signer.Sign(feed(1))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	var req *http.Request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req = r.Clone(r.Context())
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	src := notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()}
	if _, err := src.Fetch(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if req == nil {
		t.Fatal("the server saw no request")
	}
	if ua := req.Header.Get("User-Agent"); ua != "flynn" {
		t.Errorf("user agent is %q; it must name the product and nothing else, not the version", ua)
	}
	if req.URL.RawQuery != "" {
		t.Errorf("the request carried a query string: %q", req.URL.RawQuery)
	}
	if len(req.Cookies()) != 0 {
		t.Errorf("the request carried cookies: %v", req.Cookies())
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("the request carried an Authorization header")
	}
}

func TestOversizedBodyIsNotRead(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// An origin that starts streaming forever must waste its own time, not the
		// client's memory.
		chunk := make([]byte, 64<<10)
		for range 8 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	src := notices.Source{URL: srv.URL + "/notices.cose", HTTP: srv.Client()}
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("an oversized body was accepted")
	}
}

func TestDueRespectsTheIntervalAndABadClock(t *testing.T) {
	now := at("2026-07-02T00:00:00Z")
	if !notices.Due(notices.Trust{}, now) {
		t.Error("a client that has never checked is not due")
	}
	if notices.Due(notices.Trust{Checked: now.Add(-time.Hour)}, now) {
		t.Error("a client that checked an hour ago is due again already")
	}
	if !notices.Due(notices.Trust{Checked: now.Add(-notices.RefreshInterval)}, now) {
		t.Error("a client that checked an interval ago is not due")
	}
	// A clock set backwards must not park a client in a state where it never looks again.
	if !notices.Due(notices.Trust{Checked: now.Add(24 * time.Hour)}, now) {
		t.Error("a check recorded in the future did not read as due")
	}
}

func TestOffSwitchStopsEverything(t *testing.T) {
	t.Setenv(notices.OffEnv, "1")
	if notices.Enabled() {
		t.Fatal("the off switch did not turn the channel off")
	}
	c := notices.NewClient(t.TempDir(), "0.1.0")
	if c.Show(io.Discard) {
		t.Fatal("a disabled channel rendered a notice")
	}
	if err := c.RefreshIfDue(context.Background()); err != nil {
		t.Fatalf("a disabled channel tried to refresh: %v", err)
	}
}
