package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ionalpha/flynn/internal/pathglob"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

const (
	defaultGrepResults = 500
	maxGrepResults     = 5000
	maxContextLines    = 50
	// maxLineBytes keeps one minified or JSONL line from filling the context.
	maxLineBytes = 500
	// maxSearchedFile is the largest file a directory search reads, because a read
	// cannot be interrupted. A file named as path has no limit.
	maxSearchedFile = 10 << 20
	// namedSkips is how many skipped files a result names.
	namedSkips = 5
)

type grepTool struct{ s *Set }

func (grepTool) Def() llm.Tool {
	return llm.Tool{
		Name: "grep",
		Description: "Search file contents for a regular expression (RE2 syntax), recursively under an optional path. " +
			"Returns matching lines as path:line:text, and context lines as path-line-text. " +
			"Skips binary files, version control directories and paths ignored by .gitignore, unless named as path.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["pattern"],
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression to search for."},
    "path": {"type": "string", "description": "File or directory to search, relative to the working directory. Omit to search everything."},
    "glob": {"type": "string", "description": "Only search files matching this glob, as ripgrep's --glob: '*.go', '*.{ts,tsx}', 'src/**/*.py', or '!*_test.go' to exclude."},
    "ignore_case": {"type": "boolean", "description": "Match case-insensitively."},
    "context": {"type": "integer", "description": "Lines to show before and after each match (at most 50)."},
    "before_context": {"type": "integer", "description": "Lines to show before each match; overrides context."},
    "after_context": {"type": "integer", "description": "Lines to show after each match; overrides context."},
    "files_with_matches": {"type": "boolean", "description": "Return only the paths of matching files."},
    "max_results": {"type": "integer", "description": "Most matching lines (or files) to return (default 500, at most 5000)."}
  },
  "additionalProperties": false
}`),
	}
}

type grepInput struct {
	Pattern          string `json:"pattern"`
	Path             string `json:"path"`
	Glob             string `json:"glob"`
	IgnoreCase       bool   `json:"ignore_case"`
	Context          int    `json:"context"`
	BeforeContext    *int   `json:"before_context"`
	AfterContext     *int   `json:"after_context"`
	FilesWithMatches bool   `json:"files_with_matches"`
	MaxResults       int    `json:"max_results"`
}

// fileFilter is the glob argument. As in ripgrep, a pattern without a slash
// matches a name at any depth, and a leading ! excludes.
type fileFilter struct {
	pat            *pathglob.Pattern
	exclude, names bool
}

func newFileFilter(glob string) (*fileFilter, error) {
	if glob == "" {
		return nil, nil //nolint:nilnil // no filter is not an error
	}
	f := &fileFilter{}
	glob, f.exclude = strings.CutPrefix(glob, "!")
	f.names = !strings.Contains(glob, "/")
	var err error
	if f.pat, err = pathglob.Compile(glob); err != nil {
		return nil, fmt.Errorf("grep: invalid glob: %w", err)
	}
	return f, nil
}

func (f *fileFilter) matches(rel string) bool {
	if f.names {
		rel = path.Base(rel)
	}
	return f.pat.Match(rel)
}

// keepFile reports whether the filter lets a file be searched.
func (f *fileFilter) keepFile(rel string) bool {
	return f == nil || f.matches(rel) != f.exclude
}

// keepDir reports whether a directory can hold a file the filter keeps.
func (f *fileFilter) keepDir(rel string) bool {
	switch {
	case f == nil:
		return true
	case f.exclude:
		return !f.matches(rel)
	}
	return f.names || f.pat.MayContain(rel)
}

// grep collects one call's matches.
type grep struct {
	re            *regexp.Regexp
	before, after int
	filesOnly     bool
	limit         int

	out                   []string
	shown                 int
	omitted, omittedFiles int
	tooLarge              []string
	tooLargeCount         int
}

// search adds one file's matches. Past the limit it only counts them, so the
// result can say how much was left out.
func (g *grep) search(ctx context.Context, name string, data []byte) error {
	if len(data) == 0 || isBinary(data) {
		return nil
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	omitted := false
	// next is the first line not yet printed; until is where trailing context ends.
	next, until, printed := 0, 0, false
	emit := func(i int, sep byte) {
		if g.before+g.after > 0 && len(g.out) > 0 && (!printed || i > next) {
			g.out = append(g.out, "--")
		}
		g.out = append(g.out, fmt.Sprintf("%s%c%d%c%s", name, sep, i+1, sep, clipLine(trimCR(lines[i]))))
		next, printed = i+1, true
	}
	for i := range lines {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		hit := g.re.Match(trimCR(lines[i]))
		switch {
		case hit && g.filesOnly:
			if g.shown < g.limit {
				g.out = append(g.out, name)
				g.shown++
			} else {
				g.omittedFiles++
			}
			return nil
		case hit && g.shown < g.limit:
			for j := max(next, i-g.before); j < i; j++ {
				emit(j, '-')
			}
			emit(i, ':')
			g.shown++
			until = i + 1 + g.after
		default:
			if hit {
				g.omitted++
				omitted = true
			}
			if i < until {
				emit(i, '-')
			}
		}
	}
	if omitted {
		g.omittedFiles++
	}
	return nil
}

func trimCR(line []byte) []byte { return bytes.TrimSuffix(line, []byte("\r")) }

// clipLine cuts a long line at a rune boundary and says how much is missing.
func clipLine(line []byte) string {
	if len(line) <= maxLineBytes {
		return string(line)
	}
	cut := maxLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return fmt.Sprintf("%s ... [%d more bytes]", line[:cut], len(line)-cut)
}

func (g *grep) notes() []string {
	var notes []string
	switch {
	case g.filesOnly && g.omittedFiles > 0:
		notes = append(notes, fmt.Sprintf("%s%d more files not shown; raise max_results or narrow the search]", notePrefix, g.omittedFiles))
	case g.omitted > 0:
		notes = append(notes, fmt.Sprintf("%s%d more matching lines in %d files not shown; raise max_results or narrow the search]", notePrefix, g.omitted, g.omittedFiles))
	}
	if g.tooLargeCount > 0 {
		notes = append(notes, fmt.Sprintf("%sskipped %d files over %d MiB: %s; name one as path to search it]",
			notePrefix, g.tooLargeCount, maxSearchedFile>>20, strings.Join(g.tooLarge, ", ")))
	}
	return notes
}

func (g *grep) skipLarge(name string) {
	g.tooLargeCount++
	if len(g.tooLarge) < namedSkips {
		g.tooLarge = append(g.tooLarge, name)
	}
}

func (t grepTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}
	if in.IgnoreCase {
		re = regexp.MustCompile("(?i)" + in.Pattern)
	}
	filter, err := newFileFilter(in.Glob)
	if err != nil {
		return "", err
	}
	g := &grep{re: re, filesOnly: in.FilesWithMatches, limit: clamp(in.MaxResults, defaultGrepResults, maxGrepResults)}
	if !g.filesOnly {
		g.before, g.after = contextLines(in.BeforeContext, in.Context), contextLines(in.AfterContext, in.Context)
	}

	partial, err := t.s.bounded(ctx, func(ctx context.Context) error {
		return g.run(ctx, t.s.sb, cleanPath(in.Path), filter)
	})
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	return result(g.out, append(g.notes(), partial)), nil
}

// run searches base: the one file it names, or every file under it the filter
// and the ignore rules allow.
func (g *grep) run(ctx context.Context, sb sandbox.Sandbox, base string, filter *fileFilter) error {
	tr := treeFor(sb)
	root, err := tr.stat(ctx, base)
	if err != nil {
		return err
	}
	if !root.dir {
		if !root.mode.IsRegular() {
			return fmt.Errorf("%s: not a regular file", base)
		}
		data, err := sb.ReadFile(ctx, base)
		if err != nil {
			return err
		}
		return g.search(ctx, base, data)
	}
	w := walker{sb: sb, tree: tr, visit: func(n node) (bool, error) {
		if n.dir {
			return filter.keepDir(n.rel), nil
		}
		// A symlink is not followed, and reading a pipe or device can block.
		if !n.mode.IsRegular() || !filter.keepFile(n.rel) {
			return false, nil
		}
		return false, g.file(ctx, sb, n)
	}}
	return w.walk(ctx, base)
}

// file searches one file met during a walk, unless it is too large.
func (g *grep) file(ctx context.Context, sb sandbox.Sandbox, n node) error {
	if n.size > maxSearchedFile {
		g.skipLarge(n.rel)
		return nil
	}
	data, err := sb.ReadFile(ctx, n.rel)
	if err != nil {
		return nil //nolint:nilerr // an unreadable file is skipped
	}
	if len(data) > maxSearchedFile {
		g.skipLarge(n.rel)
		return nil
	}
	return g.search(ctx, n.rel, data)
}

func contextLines(side *int, both int) int {
	if side != nil {
		both = *side
	}
	return max(0, min(both, maxContextLines))
}

// isBinary reports a NUL byte in the first chunk, so grep skips binary files.
func isBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}
