// Command vocabcheck is the public-surface text hygiene gate. It scans PR
// titles and bodies, commit messages, and diff additions for phrases on a
// denylist, for identifier-style internal references, and for disallowed
// typography. The denylist ships as SHA-256 hashes of normalized phrases, so
// the gate blocks a phrase without publishing it; findings quote the input,
// which already contains the phrase.
//
// Usage:
//
//	vocabcheck [-label name] < text     scan plain text from stdin
//	vocabcheck -diff [-label name]      scan only the added lines of a unified diff
//	vocabcheck -hash "some phrase"      print the denylist line for a phrase
//
// Exit status is 1 when any finding is reported, 2 on usage errors.
package main

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

//go:embed denylist.txt
var denylistRaw string

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vocabcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	diffMode := fs.Bool("diff", false, "treat stdin as a unified diff and scan only added lines")
	label := fs.String("label", "input", "label naming the scanned input in findings")
	hashArg := fs.String("hash", "", "print the denylist entry for the given phrase and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *hashArg != "" {
		tokens := tokensOf(*hashArg)
		if len(tokens) == 0 {
			_, _ = fmt.Fprintln(stderr, "phrase normalizes to nothing")
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "%d %s\n", len(tokens), hashPhrase(tokens))
		return 0
	}

	deny, err := parseDenylist(denylistRaw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "vocabcheck: bad embedded denylist: %v\n", err)
		return 2
	}

	var findings []finding
	if *diffMode {
		findings = scanDiff(stdin, deny)
	} else {
		findings = scanText(stdin, *label, deny)
	}

	for _, f := range findings {
		_, _ = fmt.Fprintf(stdout, "%s:%d: %s\n", f.where, f.line, f.what)
	}
	if len(findings) > 0 {
		_, _ = fmt.Fprintf(stdout, "%d finding(s). Rephrase the flagged text; the gate fails closed.\n", len(findings))
		return 1
	}
	return 0
}

type finding struct {
	where string
	line  int
	what  string
}

// denylist maps a normalized-phrase hash to its token count. ns lists the
// distinct phrase lengths present, so scanning slides only the needed windows.
type denylist struct {
	hashes map[string]struct{}
	ns     []int
}

func parseDenylist(raw string) (denylist, error) {
	d := denylist{hashes: make(map[string]struct{})}
	seen := make(map[int]bool)
	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n, hash, ok := strings.Cut(line, " ")
		count, err := strconv.Atoi(n)
		if !ok || err != nil || count < 1 || len(hash) != 64 {
			return d, fmt.Errorf("line %d: want \"<words> <sha256hex>\"", i+1)
		}
		d.hashes[hash] = struct{}{}
		if !seen[count] {
			seen[count] = true
			d.ns = append(d.ns, count)
		}
	}
	return d, nil
}

// tokensOf normalizes text for phrase matching: lowercase, apostrophes
// removed, every other non-alphanumeric rune a separator. Hyphenated
// compounds therefore split, so a phrase matches however it is joined.
func tokensOf(text string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == '\'' || r == '\u2019': // ASCII and curly apostrophes
			// Drop apostrophes so contractions normalize to one token.
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

func hashPhrase(tokens []string) string {
	sum := sha256.Sum256([]byte(strings.Join(tokens, " ")))
	return hex.EncodeToString(sum[:])
}

// internalRef flags identifier-style references to internal trackers: a
// tracker noun followed by an 8-hex short id, optionally backtick-quoted.
var internalRef = regexp.MustCompile("(?i)\\b(task|epic|note|sprint|audit)s?[ :]+`?[0-9a-f]{8}\\b")

// checkLine reports every finding on one line of about-to-be-public text.
func checkLine(line string, deny denylist) []string {
	var found []string
	for _, r := range line {
		switch r {
		case '\u2014': // em-dash, escaped so the gate passes its own diff
			found = append(found, `em-dash character (U+2014); use "-", a comma, or a colon`)
		case '\u2026': // ellipsis
			found = append(found, `ellipsis character (U+2026); write "..."`)
		}
	}
	tokens := tokensOf(line)
	for _, n := range deny.ns {
		for i := 0; i+n <= len(tokens); i++ {
			if _, hit := deny.hashes[hashPhrase(tokens[i:i+n])]; hit {
				found = append(found, fmt.Sprintf("denylisted phrase %q", strings.Join(tokens[i:i+n], " ")))
			}
		}
	}
	if m := internalRef.FindString(line); m != "" {
		found = append(found, fmt.Sprintf("internal tracker reference %q", m))
	}
	return found
}

func scanText(r io.Reader, label string, deny denylist) []finding {
	var findings []finding
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lineNo := 1; sc.Scan(); lineNo++ {
		for _, what := range checkLine(sc.Text(), deny) {
			findings = append(findings, finding{where: label, line: lineNo, what: what})
		}
	}
	return findings
}

var hunkHeader = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)`)

// scanDiff scans only the added side of a unified diff: new file paths and
// "+" lines. Context and removed lines are ignored, so a PR that deletes a
// flagged phrase passes.
func scanDiff(r io.Reader, deny denylist) []finding {
	var findings []finding
	file := "diff"
	newLine := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			for _, what := range checkLine(file, deny) {
				findings = append(findings, finding{where: file, line: 0, what: what + " (in the file path)"})
			}
		case hunkHeader.MatchString(line):
			start, _ := strconv.Atoi(hunkHeader.FindStringSubmatch(line)[1])
			newLine = start
		case strings.HasPrefix(line, "+"):
			for _, what := range checkLine(line[1:], deny) {
				findings = append(findings, finding{where: file, line: newLine, what: what})
			}
			newLine++
		case strings.HasPrefix(line, "-") || strings.HasPrefix(line, "\\"):
			// Removed lines and "\ No newline" markers do not advance the new side.
		default:
			newLine++
		}
	}
	return findings
}
