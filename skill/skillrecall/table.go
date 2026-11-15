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

// Table is a pack's retrieval table: objectives each skill must be offered for, and
// objectives it must stay out of. The absent column catches a description that
// reaches into another skill's subject and crowds out the better match, which no
// author would think to test.
type Table struct {
	Cases []Case
}

// Case is one row. File and Line locate it, so a failure names a place to edit.
type Case struct {
	Objective string
	Offered   []string
	Absent    []string
	File      string
	Line      int
}

// Where returns file:line, or the line alone for a table without a file.
func (c Case) Where() string {
	if c.File == "" {
		return fmt.Sprintf("line %d", c.Line)
	}
	return fmt.Sprintf("%s:%d", c.File, c.Line)
}

// tableSep is a pipe, because objectives contain commas, colons and quotes.
const tableSep = "|"

// ParseTable reads rows of `objective | offered | absent`, with comma-separated slug
// columns that may be empty, `#` comments and blank lines. The format is plain so
// authors write rows while writing the skill. Errors name the line.
func ParseTable(data []byte) (Table, error) { return parseTable("", data) }

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

// LoadTable joins every skill's [TableFile] under root, in directory order. A
// missing file is skipped; the pack's coverage test reports those in one message.
// Each skill owns its rows so adding a skill adds a file and never conflicts with
// another branch. A row must name its own skill in a column, so the file is all the
// pack asserts about that skill.
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

func slugs(col string) []string {
	var out []string
	for _, s := range strings.Split(col, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Check runs every row through [Recall], the ranker the runtime uses, and returns one
// failure per unmet expectation in table order.
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

// Failure is one unmet expectation. Offered tells the author whether to widen one
// description or narrow another.
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

// Report renders failures one per line.
func Report(fs []Failure) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}

// Covers returns the slugs named in any column, sorted, so a pack test can refuse a
// skill with no stated trigger.
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

// Claims returns the slugs expected in some offer, sorted. It is stricter than
// [Table.Covers]: a skill named only in absent columns has no objective of its own.
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
