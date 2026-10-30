package catalog

import (
	"errors"
	"os"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func userModel(id string) ModelSpec {
	return ModelSpec{
		ID:      id,
		Name:    "mine",
		Kind:    KindLocal,
		Source:  Source{Publisher: "someone", URL: "https://huggingface.co/someone/mine", Registry: "huggingface"},
		License: "MIT",
		Quants:  []Quant{{Name: "Q4_K_M", Format: FormatGGUF, SizeBytes: 1 << 30, URL: "https://huggingface.co/someone/mine/resolve/main/m.gguf", Digest: "sha256:abc"}},
		// A file may claim whatever it likes; Merge decides the tier.
		Trust:        TrustBlessed,
		ChatTemplate: "chatml",
	}
}

func blessedID(t *testing.T) string {
	t.Helper()
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range base.Models {
		if m.Local() {
			return m.ID
		}
	}
	t.Fatal("embedded catalog has no local model")
	return ""
}

func TestMergeHoldsUserEntriesAtLocalTrust(t *testing.T) {
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	merged, refused := Merge(base, []ModelSpec{userModel("mine:1b")})
	if len(refused) != 0 {
		t.Fatalf("refused: %v", refused)
	}
	if len(merged.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want %d", len(merged.Models), len(base.Models)+1)
	}
	got := merged.Models[len(merged.Models)-1]
	if got.ID != "mine:1b" || got.Trust != TrustLocal || !got.Unverified() {
		t.Fatalf("user entry = %+v, want id mine:1b held at %q", got, TrustLocal)
	}
	for _, m := range merged.Models[:len(base.Models)] {
		if m.Trust == TrustLocal {
			t.Fatalf("embedded entry %s was demoted", m.ID)
		}
	}
	if err := merged.Validate(); err != nil {
		t.Fatalf("merged catalog does not validate: %v", err)
	}
}

// TestMergeNeverShadowsAnEmbeddedID is the guarantee the overlay exists under: whatever the
// user writes, the entry that answers to a blessed id is the blessed one.
func TestMergeNeverShadowsAnEmbeddedID(t *testing.T) {
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	id := blessedID(t)
	for name, claimed := range map[string]string{
		"exact":      id,
		"upper case": strings.ToUpper(id),
		"padded":     " " + id,
	} {
		t.Run(name, func(t *testing.T) {
			merged, refused := Merge(base, []ModelSpec{userModel(claimed)})
			if len(refused) != 1 || !strings.Contains(refused[0].Error(), claimed) {
				t.Fatalf("refusals = %v, want one naming %q", refused, claimed)
			}
			if len(merged.Models) != len(base.Models) {
				t.Fatalf("a refused entry joined the catalog")
			}
			count := 0
			for _, m := range merged.Models {
				if foldID(m.ID) == foldID(id) {
					count++
					if m.Trust != TrustBlessed {
						t.Fatalf("%s answers at trust %q", m.ID, m.Trust)
					}
				}
			}
			if count != 1 {
				t.Fatalf("%d entries answer to %s", count, id)
			}
		})
	}
}

func TestMergeRefusals(t *testing.T) {
	api := userModel("hosted:thing")
	api.Kind = KindAPI
	noLicense := userModel("nolicense:1b")
	noLicense.License = ""
	spaced := userModel("has space")
	cases := []struct {
		name string
		in   []ModelSpec
		want string
	}{
		{"hosted model", []ModelSpec{api}, "only local models"},
		{"invalid entry", []ModelSpec{noLicense}, "no license"},
		{"space in id", []ModelSpec{spaced}, "spaces"},
		{"no id", []ModelSpec{userModel("")}, "no id"},
		{"duplicate within the overlay", []ModelSpec{userModel("dup:1"), userModel("DUP:1")}, "already in the catalog"},
	}
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, refused := Merge(base, tc.in)
			if len(refused) == 0 || !strings.Contains(refused[len(refused)-1].Reason, tc.want) {
				t.Fatalf("refusals = %v, want one containing %q", refused, tc.want)
			}
		})
	}
}

func TestMergeDoesNotModifyItsInput(t *testing.T) {
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	n := len(base.Models)
	Merge(base, []ModelSpec{userModel("mine:1b")})
	if len(base.Models) != n {
		t.Fatal("Merge changed the base catalog")
	}
}

func TestAddAndRemoveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	replaced, err := AddUser(dir, userModel("mine:1b"))
	if err != nil || replaced {
		t.Fatalf("AddUser = %v, %v", replaced, err)
	}
	cat, refused, err := LoadWithOverlay(dir)
	if err != nil || len(refused) != 0 {
		t.Fatalf("LoadWithOverlay = %v, %v", refused, err)
	}
	var found bool
	for _, m := range cat.Models {
		if m.ID == "mine:1b" {
			found = m.Trust == TrustLocal
		}
	}
	if !found {
		t.Fatal("added entry is not in the merged catalog at the local tier")
	}

	// Adding the same id again replaces the entry rather than duplicating it.
	again := userModel("MINE:1b")
	again.Name = "renamed"
	replaced, err = AddUser(dir, again)
	if err != nil || !replaced {
		t.Fatalf("second AddUser = %v, %v", replaced, err)
	}
	user, err := ReadOverlay(dir)
	if err != nil || len(user) != 1 || user[0].Name != "renamed" {
		t.Fatalf("overlay = %+v, %v", user, err)
	}

	if err := RemoveUser(dir, "mine:1b"); err != nil {
		t.Fatal(err)
	}
	if user, _ := ReadOverlay(dir); len(user) != 0 {
		t.Fatalf("overlay after remove = %+v", user)
	}
	if err := RemoveUser(dir, "mine:1b"); !errors.Is(err, ErrNotUserEntry) {
		t.Fatalf("removing twice = %v, want ErrNotUserEntry", err)
	}
}

func TestAddAndRemoveLeaveOtherEntriesAlone(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"a:1b", "b:1b", "c:1b"} {
		if _, err := AddUser(dir, userModel(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveUser(dir, "B:1b"); err != nil {
		t.Fatal(err)
	}
	user, err := ReadOverlay(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range user {
		ids = append(ids, u.ID)
	}
	if got := strings.Join(ids, ","); got != "a:1b,c:1b" {
		t.Fatalf("overlay holds %q, want a:1b,c:1b", got)
	}
}

func TestOverlayFailuresAreReportedNotSwallowed(t *testing.T) {
	m := userModel("mine:1b")

	t.Run("overlay path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(OverlayPath(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadOverlay(dir); err == nil {
			t.Error("ReadOverlay accepted a directory")
		}
		if _, err := AddUser(dir, m); err == nil {
			t.Error("AddUser wrote over a directory")
		}
		if err := RemoveUser(dir, m.ID); err == nil || errors.Is(err, ErrNotUserEntry) {
			t.Errorf("RemoveUser = %v, want the read error", err)
		}
	})

	t.Run("data directory is a file", func(t *testing.T) {
		file := t.TempDir() + "/file"
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AddUser(file, m); err == nil {
			t.Error("AddUser created an overlay inside a file")
		}
	})
}

func TestAddRefusesABlessedIDAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	id := blessedID(t)
	_, err := AddUser(dir, userModel(id))
	var r RefusalError
	if !errors.As(err, &r) || !strings.Contains(err.Error(), id) {
		t.Fatalf("AddUser = %v, want a RefusalError naming %s", err, id)
	}
	if _, statErr := os.Stat(OverlayPath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("a refused add wrote the overlay (%v)", statErr)
	}
}

func TestRemoveRefusesACuratedEntry(t *testing.T) {
	dir := t.TempDir()
	id := blessedID(t)
	err := RemoveUser(dir, id)
	var r RefusalError
	if !errors.As(err, &r) || errors.Is(err, ErrNotUserEntry) {
		t.Fatalf("RemoveUser(%s) = %v, want a RefusalError", id, err)
	}
}

// TestOverlayEntryForABlessedIDInTheFileIsRefusedOnLoad covers a hand-edited file: the
// guarantee does not depend on the add command having been the writer.
func TestOverlayEntryForABlessedIDInTheFileIsRefusedOnLoad(t *testing.T) {
	dir := t.TempDir()
	id := blessedID(t)
	spoof := userModel(id)
	if err := writeOverlay(dir, []ModelSpec{spoof, userModel("ok:1b")}); err != nil {
		t.Fatal(err)
	}
	cat, refused, err := LoadWithOverlay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refused) != 1 || refused[0].ID != id {
		t.Fatalf("refused = %v, want the spoofed %s", refused, id)
	}
	for _, m := range cat.Models {
		if m.ID == id && m.Trust != TrustBlessed {
			t.Fatalf("%s now answers at trust %q", id, m.Trust)
		}
	}
	if _, ok := lookup(cat, "ok:1b"); !ok {
		t.Fatal("a valid neighbour was dropped along with the refused entry")
	}
}

func lookup(c Catalog, id string) (ModelSpec, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelSpec{}, false
}

func TestLoadWithOverlayKeepsTheCuratedListWhenTheFileIsDamaged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(OverlayPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, _, err := LoadWithOverlay(dir)
	if err == nil {
		t.Fatal("a damaged overlay was not reported")
	}
	base, _ := Load()
	if len(cat.Models) != len(base.Models) {
		t.Fatalf("got %d models, want the %d curated ones", len(cat.Models), len(base.Models))
	}
}

func TestLoadWithOverlayWithNoDirectoryOrFile(t *testing.T) {
	base, _ := Load()
	for _, dir := range []string{"", t.TempDir()} {
		cat, refused, err := LoadWithOverlay(dir)
		if err != nil || len(refused) != 0 || len(cat.Models) != len(base.Models) {
			t.Fatalf("LoadWithOverlay(%q) = %d models, %v, %v", dir, len(cat.Models), refused, err)
		}
	}
}

func TestReadOverlayRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(OverlayPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxOverlayBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := ReadOverlay(dir); err == nil {
		t.Fatal("an oversized overlay was read")
	}
}

// TestMergePropertyEmbeddedEntriesAreUntouched checks, over arbitrary user files, the two
// guarantees the overlay exists to give: every embedded entry comes through Merge exactly as
// it went in, and nothing a user supplies is held above the local tier. Ids are drawn from the
// embedded ids (in odd case and padding) and from fresh ones, so collisions are common.
func TestMergePropertyEmbeddedEntriesAreUntouched(t *testing.T) {
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(base.Models))
	for _, m := range base.Models {
		ids = append(ids, m.ID)
	}
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 6).Draw(t, "entries")
		user := make([]ModelSpec, 0, n)
		for range n {
			m := userModel("user-" + rapid.StringMatching(`[a-z0-9]{1,6}`).Draw(t, "fresh"))
			if rapid.Bool().Draw(t, "collide") {
				id := rapid.SampledFrom(ids).Draw(t, "embedded")
				switch rapid.IntRange(0, 2).Draw(t, "disguise") {
				case 1:
					id = strings.ToUpper(id)
				case 2:
					id = " " + id + " "
				}
				m.ID = id
			}
			m.Trust = Trust(rapid.SampledFrom([]string{"blessed", "local", "", "trusted"}).Draw(t, "claimed trust"))
			user = append(user, m)
		}

		merged, _ := Merge(base, user)

		for i, want := range base.Models {
			got := merged.Models[i]
			if got.ID != want.ID || got.Name != want.Name || got.Trust != want.Trust || got.License != want.License {
				t.Fatalf("embedded entry %d changed: %+v -> %+v", i, want, got)
			}
		}
		for _, m := range merged.Models[len(base.Models):] {
			if m.Trust != TrustLocal {
				t.Fatalf("user entry %q loaded at trust %q", m.ID, m.Trust)
			}
			for _, id := range ids {
				if foldID(m.ID) == foldID(id) {
					t.Fatalf("user entry %q shadows embedded id %q", m.ID, id)
				}
			}
		}
	})
}
