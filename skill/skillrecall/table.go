package skillrecall

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/state"
)

// Table is a pack's retrieval table: the objectives its authors claim each skill is
// reached for, and the objectives it must stay out of. A pack carries one as a text
// file beside its skill directories, and its test runs the real ranker over the real
// pack against it.
//
// The negative column is the one that earns the file. A description that does not
// reach its own subject makes one skill invisible; a description that reaches into
// another skill's subject crowds out the better match on every objective it steals,
// and nothing about writing that skill would prompt anyone to test for it.
type Table struct {
	Cases []Case
}

// Case is one row: an objective, the slugs that must appear in what it is offered,
// and the slugs that must not. Line is where the row was written, so a failure names
// a place to edit rather than a string to search for.
type Case struct {
	Objective string
	Offered   []string
	Absent    []string
	Line      int
}

// tableSep separates a row's three columns. A pipe, because an objective is written
// as a person would type it and commas, colons and quotes all belong inside one.
const tableSep = "|"

// ParseTable reads a retrieval table. Rows are `objective | offered | absent`, the
// two slug columns are comma-separated and either may be empty, `#` opens a comment,
// and blank lines are ignored.
//
// The format is this plain because its rows are written while a skill is being
// authored, by whoever is authoring it, and a form that needs a schema in front of it
// is a form that gets skipped. Everything it refuses is refused by name and line.
func ParseTable(data []byte) (Table, error) {
	var t Table
	for i, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := raw
		if h := strings.Index(line, "#"); h >= 0 {
			line = line[:h]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, tableSep)
		if len(cols) > 3 {
			return Table{}, fmt.Errorf("line %d: %d columns, want objective%soffered%sabsent", i+1, len(cols), tableSep, tableSep)
		}
		c := Case{Objective: strings.TrimSpace(cols[0]), Line: i + 1}
		if c.Objective == "" {
			return Table{}, fmt.Errorf("line %d: no objective", i+1)
		}
		if len(cols) > 1 {
			c.Offered = slugs(cols[1])
		}
		if len(cols) > 2 {
			c.Absent = slugs(cols[2])
		}
		if len(c.Offered) == 0 && len(c.Absent) == 0 {
			return Table{}, fmt.Errorf("line %d: %q expects nothing, so it asserts nothing", i+1, c.Objective)
		}
		t.Cases = append(t.Cases, c)
	}
	return t, nil
}

// slugs splits and trims a comma-separated slug column.
func slugs(col string) []string {
	var out []string
	for _, s := range strings.Split(col, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Check runs every row against the library in skills and returns one failure per
// unmet expectation, in the table's own order. An empty result is a pack whose
// descriptions reach what their authors said they reach.
//
// The ranker it runs is the one the runtime runs, over the store the runtime reads,
// so a row passing here is the same event as the skill being offered in production.
func (t Table) Check(ctx context.Context, skills state.SkillStore, limit int) []Failure {
	var out []Failure
	for _, c := range t.Cases {
		offered := Recall(ctx, skills, c.Objective, limit)
		got := make([]string, 0, len(offered))
		for _, s := range offered {
			got = append(got, s.Slug)
		}
		for _, want := range c.Offered {
			if !contains(got, want) {
				out = append(out, Failure{
					Case: c, Slug: want, Offered: got,
					Reason: "was not offered, so this objective would never reach it",
				})
			}
		}
		for _, unwanted := range c.Absent {
			if contains(got, unwanted) {
				out = append(out, Failure{
					Case: c, Slug: unwanted, Offered: got,
					Reason: "was offered, so its description reaches into an objective that is not its own",
				})
			}
		}
	}
	return out
}

// Failure is one unmet expectation: which row, which skill, why, and what the
// objective was actually offered, because the last of those is what tells an author
// whether to widen one description or narrow another.
type Failure struct {
	Case    Case
	Slug    string
	Reason  string
	Offered []string
}

func (f Failure) String() string {
	offered := "nothing"
	if len(f.Offered) > 0 {
		offered = strings.Join(f.Offered, ", ")
	}
	return fmt.Sprintf("line %d: %q: %s %s (offered: %s)", f.Case.Line, f.Case.Objective, f.Slug, f.Reason, offered)
}

// Report renders failures as one line each, for a test that has to say what to fix.
func Report(fs []Failure) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}

// Covers returns the slugs the table names in any column, sorted. A pack test uses
// it to refuse a skill nobody stated a trigger for: an unlisted skill is one whose
// author never had to answer what it is reached for, which is the question this file
// exists to force before a body is written.
func (t Table) Covers() []string {
	seen := map[string]bool{}
	for _, c := range t.Cases {
		for _, s := range append(append([]string{}, c.Offered...), c.Absent...) {
			seen[s] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Claims returns the slugs the table expects to be offered somewhere, sorted. It is
// the stronger of the two coverage questions: a skill named only in absent columns
// has been kept out of other people's objectives without anyone stating one of its
// own.
func (t Table) Claims() []string {
	seen := map[string]bool{}
	for _, c := range t.Cases {
		for _, s := range c.Offered {
			seen[s] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
