package bundled

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// routerSlug is the skill whose "Which skill to load" table sends a run to every other
// skill in the pack.
const routerSlug = "software-quality"

// loadCell is the last cell of a table row when it names one skill.
var loadCell = regexp.MustCompile("\\|\\s*`([a-z0-9-]+)`\\s*\\|\\s*$")

// The table is how a run that loaded only software-quality finds the narrower skill, so
// a skill missing from it is never routed to and a renamed one routes nowhere. Each
// row also restates its skill's main rule, which no test can compare; this one holds
// the part that can be checked: every row names a skill the pack ships, once, and every
// other skill has a row.
func TestRouterTableNamesEveryBundledSkill(t *testing.T) {
	skills, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	var want []string
	for _, sk := range skills {
		if sk.Slug == routerSlug {
			body = sk.Body
			continue
		}
		want = append(want, sk.Slug)
	}
	if body == "" {
		t.Fatalf("the pack has no %s skill", routerSlug)
	}
	_, table, ok := strings.Cut(body, "## Which skill to load")
	if !ok {
		t.Fatalf("%s has no \"Which skill to load\" section", routerSlug)
	}
	var got []string
	for _, line := range strings.Split(table, "\n") {
		if m := loadCell.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			got = append(got, m[1])
		}
	}
	for _, slug := range got {
		if !slices.Contains(want, slug) {
			t.Errorf("the table loads %q, which the pack does not ship", slug)
		}
	}
	for _, slug := range want {
		switch n := countOf(got, slug); n {
		case 0:
			t.Errorf("%s has no row in the %s table, so no run is sent to it", slug, routerSlug)
		case 1:
		default:
			t.Errorf("%s has %d rows in the %s table", slug, n, routerSlug)
		}
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
