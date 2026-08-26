package skillrecall

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/state"
)

// Table is a pack's retrieval table: the objectives its authors claim each skill is
// reached for, and the objectives it must stay out of. Each skill carries its own
// rows in its own directory and LoadTable assembles them, so the pack's test runs the
// real ranker over the real pack against every row.
//
// The negative column is the one that earns the file. A description that does not
// reach its own subject makes one skill invisible; a description that reaches into
// another skill's subject crowds out the better match on every objective it steals,
// and nothing about writing that skill would prompt anyone to test for it.
type Table struct {
	Cases []Case
}

// Case is one row: an objective, the slugs that must appear in what it is offered,
// and the slugs that must not. File and Line are where the row was written, so a
// failure names a place to edit rather than a string to search for.
type Case struct {
	Objective string
	Offered   []string
	Absent    []string
	File      string
	Line      int
}

// Where is the row's address, for a message someone has to act on. A table read from
// a file names it, because a pack has one file per skill and the line number alone
// would say which of eleven to open only by accident.
func (c Case) Where() string {
	if c.File == "" {
		return fmt.Sprintf("line %d", c.Line)
	}
	return fmt.Sprintf("%s:%d", c.File, c.Line)
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
func ParseTable(data []byte) (Table, error) { return parseTable("", data) }

// parseTable is ParseTable with the file the rows came from, which every row carries
// so a failure can be read as an address.
func parseTable(file string, data []byte) (Table, error) {
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
		c := Case{Objective: strings.TrimSpace(cols[0]), File: file, Line: i + 1}
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

// TableFile is the name a skill directory gives its own retrieval rows.
const TableFile = "retrieval.txt"

// LoadTable reads the retrieval rows of every skill under root and returns them as
// one table, in directory order. A skill without the file contributes nothing and is
// not an error here: what refuses a skill that states no objective is the pack's own
// coverage test, which can say so in one message instead of one per missing file.
//
// The rows live with the skill rather than in a table beside the pack because of what
// the two shapes do to the people writing them. One file gives every branch that adds
// a skill the same last line to append to, so every such branch conflicts with every
// other one and the resolution is by hand, on text no reviewer reads. Rows in the
// skill's own directory make adding a skill an added file, which nothing else touches.
//
// The cost of splitting is that the rows no longer sit next to each other, and reading
// one skill's claims beside another's is how a negative column gets written. That is
// paid back by the rule this enforces: a row in a skill's directory must name that
// skill in one of its columns, so the file is the whole of what the pack asserts about
// it, and a row that wandered is refused by address.
func LoadTable(fsys fs.FS, root string) (Table, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return Table{}, fmt.Errorf("skillrecall: read %s: %w", root, err)
	}
	var out Table
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		file := path.Join(root, slug, TableFile)
		data, err := fs.ReadFile(fsys, file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Table{}, fmt.Errorf("skillrecall: read %s: %w", file, err)
		}
		t, err := parseTable(file, data)
		if err != nil {
			return Table{}, fmt.Errorf("skillrecall: %s: %w", file, err)
		}
		for _, c := range t.Cases {
			if !contains(c.Offered, slug) && !contains(c.Absent, slug) {
				return Table{}, fmt.Errorf("skillrecall: %s: %q says nothing about %s; a row belongs to the skill it asserts about", c.Where(), c.Objective, slug)
			}
		}
		out.Cases = append(out.Cases, t.Cases...)
	}
	return out, nil
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
	return fmt.Sprintf("%s: %q: %s %s (offered: %s)", f.Case.Where(), f.Case.Objective, f.Slug, f.Reason, offered)
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
