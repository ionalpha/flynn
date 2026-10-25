package embeddedspecs_test

import (
	"sort"
	"testing"
	"testing/fstest"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/embeddedspecs"
)

// TestParseOrderAndReservedAgreeForAnyNameSet holds for every set of file names: the entries
// come back sorted by name, and the reserved set is exactly the names of the entries. A sync
// is only deterministic, and the reserved check only sound, if both hold for names nobody
// thought to write an example for.
func TestParseOrderAndReservedAgreeForAnyNameSet(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		names := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z][a-z0-9-]{0,11}`), 1, 12, func(s string) string { return s }).Draw(t, "names")
		fsys := fstest.MapFS{}
		for _, n := range names {
			fsys["specs/"+n+".json"] = &fstest.MapFile{Data: []byte(`{"bin":"` + n + `"}`)}
		}
		entries, reserved, err := embeddedspecs.Parse[spec](fsys, naming, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(entries))
		for i, e := range entries {
			got[i] = e.Name
			if e.Spec.Bin != e.Name || !reserved[e.Name] {
				t.Fatalf("entry %+v is not decoded from its own file or not reserved", e)
			}
		}
		if !sort.StringsAreSorted(got) || len(got) != len(names) || len(reserved) != len(names) {
			t.Fatalf("entries %v, reserved %v: want %d sorted names", got, reserved, len(names))
		}
	})
}
