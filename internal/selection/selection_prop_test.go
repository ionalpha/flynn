package selection

import (
	"testing"

	"pgregory.net/rapid"
)

// A slug in Deny is never permitted, whatever Bare and Allow say.
func TestDenyAlwaysWins(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		slug := rapid.StringMatching(`[a-z]{1,6}`).Draw(t, "slug")
		c := Config{
			Bare:     rapid.Bool().Draw(t, "bare"),
			AllowSet: true,
			Allow:    rapid.SliceOf(rapid.StringMatching(`[a-z]{1,6}`)).Draw(t, "allow"),
			Deny:     append(rapid.SliceOf(rapid.StringMatching(`[a-z]{1,6}`)).Draw(t, "deny"), slug),
		}
		if c.PermitsSkill(slug) {
			t.Fatalf("denied slug %q was permitted by %+v", slug, c)
		}
	})
}
