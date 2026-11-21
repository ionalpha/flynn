package bundled

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// TestEveryBundledSkillIsInThePackHistory fails when a skill's current content has no
// line in pack_history.txt. Without the line, a later release would take this version
// for a host's edit on an install that has no pack record, and keep it instead of
// replacing it.
func TestEveryBundledSkillIsInThePackHistory(t *testing.T) {
	shipped, err := shippedDigests()
	if err != nil {
		t.Fatal(err)
	}
	skills, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range skills {
		if _, ok := shipped[digest(sk)]; !ok {
			t.Errorf("%s: its current digest %s is not in pack_history.txt; run ./dev/packhistory and commit the result", sk.Slug, digest(sk))
		}
	}
}

// TestPackHistoryIsWellFormed holds the file to the form dev/packhistory writes, so a
// hand edit or a bad merge shows as a failing test and not as a silently shorter list.
func TestPackHistoryIsWellFormed(t *testing.T) {
	if _, err := parseHistory(packHistory); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(packHistory, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatal("pack_history.txt lists no versions")
	}
	if !slices.IsSorted(lines) {
		t.Error("pack_history.txt is not sorted; run ./dev/packhistory")
	}
	if got := len(slices.Compact(slices.Clone(lines))); got != len(lines) {
		t.Errorf("pack_history.txt has %d duplicate lines", len(lines)-got)
	}
}

func TestParseHistoryRejectsAMalformedLine(t *testing.T) {
	good := strings.Repeat("a", 64)
	for name, text := range map[string]string{
		"no digest":   "slug\n",
		"short":       "slug abc\n",
		"three words": "slug " + good + " extra\n",
	} {
		if _, err := parseHistory(text); err == nil {
			t.Errorf("%s: accepted %q", name, text)
		}
	}
	got, err := parseHistory("# comment\n\nslug " + good + "\n")
	if err != nil || len(got) != 1 {
		t.Errorf("a valid file parsed to %v, %v", got, err)
	}
}

// A copy with no pack record is the state of every install that first ran an earlier
// release. Seed must replace a copy of the pack's own content and keep anything else.
func TestSeedOnAnInstallWithNoRecords(t *testing.T) {
	ctx := context.Background()
	want, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	for _, sk := range want {
		sk.Scope = state.BundledScope
		if _, err := store.Upsert(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	edited := want[0].Slug
	editSkill(t, store, edited, "the host's wording")

	res, err := Seed(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(res.Diverged, []string{edited}) || len(res.Updated) != 0 {
		t.Errorf("first start reported %+v, want only %s diverged", res, edited)
	}
	if got, _ := store.Get(ctx, edited); got.Body != "the host's wording" || !got.Diverged {
		t.Errorf("the edit was not kept and marked: body %q diverged %v", got.Body, got.Diverged)
	}
	for _, sk := range want[1:] {
		got, _ := store.Get(ctx, sk.Slug)
		if got.Diverged || got.PackDigest == "" {
			t.Errorf("%s, an untouched copy, has diverged %v record %q", sk.Slug, got.Diverged, got.PackDigest)
		}
	}
}
