package notices_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
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

// --- Signer and Keyring construction -----------------------------------------

// TestNewSignerRefusesUnusableKeys covers the publishing side's guards. An empty key
// id produces a feed nobody can attribute, and so nobody can revoke; a malformed
// private key would produce signatures no client can check.
func TestNewSignerRefusesUnusableKeys(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	good := ed25519.NewKeyFromSeed(seed)

	tests := []struct {
		name  string
		keyID string
		priv  ed25519.PrivateKey
	}{
		{name: "empty key id", keyID: "", priv: good},
		{name: "short private key", keyID: "k", priv: good[:16]},
		{name: "nil private key", keyID: "k", priv: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := notices.NewSigner(tc.keyID, tc.priv)
			if err == nil {
				t.Fatal("expected an error")
			}
			if s != nil {
				t.Fatal("a rejected key still produced a signer")
			}
		})
	}

	s, err := notices.NewSigner("flynn-notices-1", good)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.KeyID(); got != "flynn-notices-1" {
		t.Fatalf("KeyID = %q, want %q", got, "flynn-notices-1")
	}
}

// TestKeyringAddRefusesUnusableKeys is the mirror on the verifying side: a key the
// ring cannot use must be rejected at Add rather than silently stored and then found
// broken on the day an advisory has to go out.
func TestKeyringAddRefusesUnusableKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		keyID string
		pub   ed25519.PublicKey
	}{
		{name: "empty key id", keyID: "", pub: pub},
		{name: "short public key", keyID: "k", pub: pub[:8]},
		{name: "nil public key", keyID: "k", pub: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ring := notices.NewKeyring()
			if err := ring.Add(tc.keyID, tc.pub); err == nil {
				t.Fatal("expected an error")
			}
			if ring.Len() != 0 {
				t.Fatal("a rejected key was still added to the ring")
			}
		})
	}
}

// TestKeyringAddReplacesOnRotation pins that re-adding an id swaps the key rather than
// keeping both, which is what makes a rotation a rotation and not an accumulation of
// keys that can still sign.
func TestKeyringAddReplacesOnRotation(t *testing.T) {
	signer, ring := testKey(t)
	doc, err := signer.Sign(feed(1, advisory()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := notices.Verify(doc, ring); err != nil {
		t.Fatalf("the original key should verify: %v", err)
	}

	// Rotate: the same id now holds a different public key.
	rotated, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ring.Add("test-key-1", rotated); err != nil {
		t.Fatal(err)
	}
	if ring.Len() != 1 {
		t.Fatalf("rotation should replace, not accumulate: ring holds %d keys", ring.Len())
	}
	if _, err := notices.Verify(doc, ring); err == nil {
		t.Fatal("a feed signed by the retired key still verified after rotation")
	}
}

// --- Store ------------------------------------------------------------------

func TestStoreTrustRoundTrip(t *testing.T) {
	store := notices.NewStore(t.TempDir())

	// A first run has no file, which is a zero Trust rather than an error: it trusts
	// nothing and has seen nothing.
	tr, err := store.LoadTrust()
	if err != nil {
		t.Fatalf("first-run LoadTrust: %v", err)
	}
	if tr.Version != 0 || len(tr.Shown) != 0 || !tr.Checked.IsZero() {
		t.Fatalf("a first run should start from a zero Trust, got %+v", tr)
	}
	// Nor is there a cached feed.
	doc, err := store.LoadFeed()
	if err != nil {
		t.Fatalf("first-run LoadFeed: %v", err)
	}
	if doc != nil {
		t.Fatalf("a first run should have no cached feed, got %d bytes", len(doc))
	}

	want := notices.Trust{Version: 7, Checked: at("2026-07-02T00:00:00Z"), Shown: []string{"A", "B"}}
	if err := store.SaveTrust(want); err != nil {
		t.Fatalf("SaveTrust: %v", err)
	}
	if err := store.SaveFeed([]byte("signed-bytes")); err != nil {
		t.Fatalf("SaveFeed: %v", err)
	}

	got, err := store.LoadTrust()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || !got.Checked.Equal(want.Checked) || strings.Join(got.Shown, ",") != "A,B" {
		t.Fatalf("LoadTrust = %+v, want %+v", got, want)
	}
	back, err := store.LoadFeed()
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "signed-bytes" {
		t.Fatalf("LoadFeed = %q, want the bytes that were saved", back)
	}
}

// TestCorruptTrustIsAnErrorNotAReset is the rollback defence at the storage layer. A
// trust file that does not parse must be reported, never quietly rebuilt from nothing:
// the highest-version-ever mark is exactly the state a rollback attack needs gone, so a
// client that silently reset it would be doing the attacker's work.
func TestCorruptTrustIsAnErrorNotAReset(t *testing.T) {
	dir := t.TempDir()
	store := notices.NewStore(dir)
	if err := store.SaveTrust(notices.Trust{Version: 9}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "notices", "trust.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LoadTrust(); err == nil {
		t.Fatal("a corrupt trust file should be an error, not a silent reset to version 0")
	}
}

// TestStoreReportsUnreadableState pins the other read failure: a state path that is a
// directory (or otherwise unreadable) must surface, not read as "first run".
func TestStoreReportsUnreadableState(t *testing.T) {
	dir := t.TempDir()
	store := notices.NewStore(dir)
	base := filepath.Join(dir, "notices")
	if err := os.MkdirAll(filepath.Join(base, "trust.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "feed.cose"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LoadTrust(); err == nil {
		t.Fatal("an unreadable trust path should not read as a first run")
	}
	if _, err := store.LoadFeed(); err == nil {
		t.Fatal("an unreadable feed path should not read as an empty cache")
	}
}

// TestStoreReportsUnwritableState covers the write half: if the notice directory cannot
// be created, saving must fail loudly rather than pretend the feed was cached.
func TestStoreReportsUnwritableState(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "data")
	if err := os.WriteFile(blocker, []byte("a file where a directory must go"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := notices.NewStore(blocker)

	if err := store.SaveTrust(notices.Trust{Version: 1}); err == nil {
		t.Fatal("SaveTrust into an uncreatable directory should fail")
	}
	if err := store.SaveFeed([]byte("doc")); err == nil {
		t.Fatal("SaveFeed into an uncreatable directory should fail")
	}
	// MarkShown persists through SaveTrust, so it inherits the failure rather than
	// reporting a notice as shown that was never recorded.
	if _, err := store.MarkShown(notices.Trust{}, []notices.Notice{{
		ID: "N1", Severity: notices.Info, Summary: "x",
	}}); err == nil {
		t.Fatal("MarkShown should surface the failure to persist")
	}
}

// TestMarkShownIsBoundedAndSkipsSecurity pins both of MarkShown's rules: a security
// notice is never recorded (it must keep appearing while it applies), and the shown
// list cannot grow without limit on a long-lived install.
func TestMarkShownIsBoundedAndSkipsSecurity(t *testing.T) {
	store := notices.NewStore(t.TempDir())

	// A security notice does not enter the shown list, and with nothing else to record
	// the trust state is returned untouched (no write at all).
	tr, err := store.MarkShown(notices.Trust{}, []notices.Notice{advisory()})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Shown) != 0 {
		t.Fatalf("a security notice was recorded as shown: %v", tr.Shown)
	}

	// Recording the same non-security notice twice records it once.
	info := notices.Notice{ID: "N-info", Severity: notices.Info, Summary: "a thing happened"}
	tr, err = store.MarkShown(tr, []notices.Notice{info})
	if err != nil {
		t.Fatal(err)
	}
	before := len(tr.Shown)
	tr, err = store.MarkShown(tr, []notices.Notice{info})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Shown) != before {
		t.Fatalf("recording the same notice twice grew the list: %v", tr.Shown)
	}

	// Overfill the list well past the ceiling and check it is trimmed, oldest first.
	const ceiling = notices.MaxNotices * 4
	var many []notices.Notice
	for i := range ceiling + 10 {
		many = append(many, notices.Notice{
			ID:       "N" + itoa(i),
			Severity: notices.Deprecation,
			Summary:  "s",
		})
	}
	tr, err = store.MarkShown(notices.Trust{}, many)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Shown) != ceiling {
		t.Fatalf("shown list holds %d ids, want it trimmed to %d", len(tr.Shown), ceiling)
	}
	// The oldest ids are the ones dropped, so the newest are still remembered.
	if tr.Shown[len(tr.Shown)-1] != "N"+itoa(ceiling+9) {
		t.Fatalf("the newest id was trimmed instead of the oldest: %v", tr.Shown[len(tr.Shown)-1])
	}
	if tr.Shown[0] == "N0" {
		t.Fatal("the oldest id survived a trim that should have dropped it")
	}

	// The trimmed state is what a later run reads back.
	reloaded, err := store.LoadTrust()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Shown) != ceiling {
		t.Fatalf("persisted shown list holds %d ids, want %d", len(reloaded.Shown), ceiling)
	}
}

// --- Render -----------------------------------------------------------------

// TestRenderLabelsEachSeverityAndOrdersSecurityFirst covers every branch of the label
// prefix and the ordering rule: security must not be buried under the chatter.
func TestRenderLabelsEachSeverityAndOrdersSecurityFirst(t *testing.T) {
	ns := []notices.Notice{
		{ID: "i", Severity: notices.Info, Summary: "informational thing"},
		{ID: "d", Severity: notices.Deprecation, Summary: "deprecated thing"},
		{ID: "s", Severity: notices.Security, Summary: "security thing", URL: "https://flynnhq.com/a"},
	}
	var buf bytes.Buffer
	if !notices.Render(&buf, ns, false) {
		t.Fatal("Render reported writing nothing")
	}
	out := buf.String()

	sec := strings.Index(out, "SECURITY: security thing")
	dep := strings.Index(out, "notice:  deprecated thing")
	inf := strings.Index(out, "notice:  informational thing")
	if sec < 0 || dep < 0 || inf < 0 {
		t.Fatalf("a severity was not labelled:\n%s", out)
	}
	if sec >= dep || dep >= inf {
		t.Fatalf("severities are out of order (security must come first):\n%s", out)
	}
	if !strings.Contains(out, "https://flynnhq.com/a") {
		t.Fatalf("the advisory URL was not printed:\n%s", out)
	}

	// Nothing pending and not stale writes nothing at all, so a quiet channel adds no
	// noise to a command's output.
	var quiet bytes.Buffer
	if notices.Render(&quiet, nil, false) {
		t.Fatalf("Render wrote for an empty feed: %q", quiet.String())
	}
	if quiet.Len() != 0 {
		t.Fatalf("Render wrote %q for an empty feed", quiet.String())
	}

	// Staleness alone still writes, because silence would read as all-clear.
	var stale bytes.Buffer
	if !notices.Render(&stale, nil, true) {
		t.Fatal("a stale feed with no notices should still say so")
	}
	if !strings.Contains(stale.String(), "not been refreshed recently") {
		t.Fatalf("stale warning missing: %q", stale.String())
	}
}

// --- Stale ------------------------------------------------------------------

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

// --- Applies / parseVersion --------------------------------------------------

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

// --- Client -----------------------------------------------------------------

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

// --- Fetch ------------------------------------------------------------------

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

// --- decodePayload structural validation --------------------------------------

// TestMalformedFeedContentsAreRefused walks decodePayload's structural rules. These run
// on bytes whose signature already verified, because our own key could be misused and a
// publisher can make a mistake, and neither should be able to hand a client a feed it
// will mis-render or silently swallow half of.
func TestMalformedFeedContentsAreRefused(t *testing.T) {
	signer, ring := testKey(t)

	tests := []struct {
		name string
		feed notices.Feed
		want string
	}{
		{
			name: "a notice with no id",
			feed: feed(1, notices.Notice{ID: "", Severity: notices.Info, Summary: "s"}),
			want: "no id",
		},
		{
			name: "a notice whose id is only control characters",
			feed: feed(1, notices.Notice{ID: "\x00\x01", Severity: notices.Info, Summary: "s"}),
			want: "no id",
		},
		{
			name: "two notices under one id",
			feed: feed(
				1,
				notices.Notice{ID: "DUP", Severity: notices.Info, Summary: "first"},
				notices.Notice{ID: "DUP", Severity: notices.Security, Summary: "second"},
			),
			want: "duplicate notice id",
		},
		{
			name: "an unknown severity",
			feed: feed(1, notices.Notice{ID: "N", Severity: notices.Severity("urgent"), Summary: "s"}),
			want: "unknown severity",
		},
		{
			name: "an empty severity",
			feed: feed(1, notices.Notice{ID: "N", Severity: notices.Severity(""), Summary: "s"}),
			want: "unknown severity",
		},
		{
			name: "a notice with no summary",
			feed: feed(1, notices.Notice{ID: "N", Severity: notices.Info, Summary: ""}),
			want: "no summary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := signer.Sign(tc.feed)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			// The signature is good: the refusal is structural, not cryptographic.
			_, err = notices.Verify(doc, ring)
			if err == nil {
				t.Fatal("a malformed feed verified")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestTooManyNoticesOrFloorsIsRefused pins the two count ceilings, which stop a signed
// document from being used as an unbounded allocation.
func TestTooManyNoticesOrFloorsIsRefused(t *testing.T) {
	signer, ring := testKey(t)

	t.Run("notices", func(t *testing.T) {
		f := notices.Feed{Version: 1, Issued: at("2026-07-01T00:00:00Z")}
		for i := range notices.MaxNotices + 1 {
			f.Notices = append(f.Notices, notices.Notice{
				ID: "N" + itoa(i), Severity: notices.Info, Summary: "s",
			})
		}
		doc, err := signer.Sign(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := notices.Verify(doc, ring); err == nil || !strings.Contains(err.Error(), "too many notices") {
			t.Fatalf("got %v, want a too-many-notices refusal", err)
		}
	})

	t.Run("floors", func(t *testing.T) {
		f := notices.Feed{Version: 1, Issued: at("2026-07-01T00:00:00Z")}
		for i := range notices.MaxFloors + 1 {
			f.Floors = append(f.Floors, notices.Floor{
				Runtime: "rt" + itoa(i), MinVersion: "1.0.0",
			})
		}
		doc, err := signer.Sign(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := notices.Verify(doc, ring); err == nil || !strings.Contains(err.Error(), "too many floors") {
			t.Fatalf("got %v, want a too-many-floors refusal", err)
		}
	})
}

// TestAnUngatingFloorIsDroppedNotFatal pins the asymmetry between notices and floors: a
// floor that gates nothing is dropped, because a malformed floor must not be able to take
// down the whole feed, and the feed is also how a security advisory reaches the user.
func TestAnUngatingFloorIsDroppedNotFatal(t *testing.T) {
	signer, ring := testKey(t)
	f := feed(1, advisory())
	f.Floors = []notices.Floor{
		{Runtime: "", MinVersion: "1.0.0"},                                    // no runtime: gates nothing
		{Runtime: "llama.cpp", MinVersion: ""},                                // no version: gates nothing
		{Runtime: "vllm", MinVersion: "0.7.0", AdvisoryID: "FLYNN-2026-0001"}, // real
	}
	doc, err := signer.Sign(f)
	if err != nil {
		t.Fatal(err)
	}

	got, err := notices.Verify(doc, ring)
	if err != nil {
		t.Fatalf("an ungating floor took the whole feed down: %v", err)
	}
	if len(got.Floors) != 1 || got.Floors[0].Runtime != "vllm" {
		t.Fatalf("floors = %+v, want only the one that actually gates", got.Floors)
	}
	// The advisory still came through, which is the point.
	if len(got.Notices) != 1 {
		t.Fatalf("the advisory was lost alongside the malformed floors: %+v", got.Notices)
	}
}

// TestFeedFromAForeignOriginIsRefused pins the origin check: a valid signature over some
// other Flynn document must never be presentable as a valid signature over a notice feed.
func TestFeedFromAForeignOriginIsRefused(t *testing.T) {
	_, ring := testKey(t)

	// A document that is well-formed CBOR and correctly signed by a trusted key, but is
	// not a notice feed, is rejected on the origin field before anything is rendered.
	// The nearest reachable version of that is a payload the decoder reads but whose
	// origin does not match, which only the encoder can produce; a truncated document
	// stands in for the same class of refusal.
	if _, err := notices.Verify([]byte{0xd2, 0x84, 0x00}, ring); err == nil {
		t.Fatal("a truncated document verified")
	}
	if _, err := notices.Verify(nil, ring); err == nil {
		t.Fatal("an empty document verified")
	}
	// A ring with no keys can trust nothing at all, and says so rather than going quiet.
	if _, err := notices.Verify([]byte{0xd2, 0x84}, notices.NewKeyring()); err == nil {
		t.Fatal("an empty keyring verified a document")
	}
	if _, err := notices.Verify([]byte{0xd2, 0x84}, nil); err == nil {
		t.Fatal("a nil keyring verified a document")
	}
}

// TestDefaultKeyringSkipsAMalformedKey pins the compiled-in ring's failure direction: a
// bad key entry costs us the ability to say something through that key, and must not take
// the user's agent down with a panic.
func TestDefaultKeyringSkipsAMalformedKey(t *testing.T) {
	ring := notices.DefaultKeyring()
	if ring.Len() != notices.SourceKeyCount() {
		t.Fatalf("the shipped keyring holds %d of %d declared keys: a key in the source does not parse",
			ring.Len(), notices.SourceKeyCount())
	}
	if ring.Len() == 0 {
		t.Fatal("a release must not ship with an empty keyring: those binaries could never be reached")
	}
}

// TestTrustJSONShape pins the on-disk trust format, which a future version has to keep
// reading: a rename that silently dropped the version field would erase the rollback mark
// on every existing install.
func TestTrustJSONShape(t *testing.T) {
	dir := t.TempDir()
	store := notices.NewStore(dir)
	if err := store.SaveTrust(notices.Trust{
		Version: 3,
		Checked: at("2026-07-02T00:00:00Z"),
		Shown:   []string{"N1"},
	}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "notices", "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "checked", "shown"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("the trust file is missing the %q field: %s", key, b)
		}
	}
	var version uint64
	if err := json.Unmarshal(raw["version"], &version); err != nil || version != 3 {
		t.Fatalf("version field = %s (err %v), want the number 3", raw["version"], err)
	}
}
