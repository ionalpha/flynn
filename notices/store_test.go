package notices_test

// The on-disk state: the trust file and the record of what has already been shown. Two
// rules hold it together. A file the store cannot read or write is an error, never a
// silent reset, because resetting trust is how a rollback mark gets erased. And the
// on-disk shape is pinned, because a future version has to keep reading what this one
// wrote.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

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
