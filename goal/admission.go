package goal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ionalpha/flynn/resource"
)

// A ledger check is written by the model before any work, and a check that is wrong
// proves nothing in one of four ways: it reads a file nobody was asked to make, it names a
// place the run's confined commands cannot write, it cannot run as written, or it already
// passes on the tree as it stands. The first two are decided from the text of the check, the
// last two by running it once on the untouched tree. Admission is the rule that decides all
// four before work starts, so a wrong check goes back to the planner and not into a run that
// then stalls on it or, worse, proves an item with a check that was never able to fail.

// AdmissionRefusal names why a check was not admitted. The values are recorded on the run's
// spine, so they are stable strings and not prose.
type AdmissionRefusal string

const (
	// RefusedUngrounded means the check names a path that does not exist before the work and that
	// the objective never asks for, so the only thing that could make it pass is the worker
	// creating the very file the check looks for.
	RefusedUngrounded AdmissionRefusal = "ungrounded"
	// RefusedUnwritable means the check names a path outside the working directory that does not
	// exist. The run's commands are confined and cannot create it, so the check can never
	// pass.
	RefusedUnwritable AdmissionRefusal = "unwritable"
	// RefusedMalformed means the shell rejected the check's text (syntax, quoting).
	RefusedMalformed AdmissionRefusal = "malformed"
	// RefusedMissingProgram means the check runs a program the host does not have.
	RefusedMissingProgram AdmissionRefusal = "missing_program"
	// RefusedAlreadyPasses means the check exits 0 on the tree before any work, so a pass later
	// distinguishes nothing.
	RefusedAlreadyPasses AdmissionRefusal = "already_passes"
)

// AdmissionVerdict is the outcome of admitting one ledger item's check. Item is the item's
// content address ([ItemID]), so the verdict names the exact check it judged.
type AdmissionVerdict struct {
	Item     string
	Admitted bool
	// Refusal is set when Admitted is false.
	Refusal AdmissionRefusal
	// Reason is the sentence the planner is sent back, and on an admitted verdict a note
	// when something was not checked (the dry run could not execute).
	Reason string
	// DryRun reports whether the check was executed once on the untouched tree. An admitted
	// verdict with DryRun false was admitted on its text alone.
	DryRun bool
}

// AdmissionRecorder writes admission verdicts onto the run's durable record. It is a
// separate port from [Evidence] so that adding it does not change what a host implementing
// Evidence must provide; the shipped implementation is evidence.SpineEvidence.
type AdmissionRecorder interface {
	RecordAdmission(ctx context.Context, r resource.Resource, v AdmissionVerdict) error
}

// Ground is what grounding a check is judged against.
type Ground struct {
	// Dir is the working directory a check runs in. Empty disables grounding, because a
	// host-supplied sandbox has no directory the run chose to look in.
	Dir string
	// Confined reports that the run's commands cannot write outside Dir.
	Confined bool
	// Objective is the text of the goal (objective and stop condition) a path may be named in.
	Objective string
	// LookPath resolves a bare file name on the host's program search path.
	LookPath func(string) (string, error)
}

// GroundingProblem is one path in a check that admission refuses.
type GroundingProblem struct {
	Path    string
	Refusal AdmissionRefusal
	Reason  string
}

// CheckGrounding returns the paths in check that admission refuses, in order and once each.
//
// A path the check reads must exist before the work or be something the objective asks for.
// Without that a planner can invent an artifact ("result.txt exists") that the worker then
// creates to satisfy it, and the item reads as proven by a check that would have passed
// whatever the work was. A path outside the working directory that does not exist is
// refused whatever the objective says when the run is confined, because no command of the
// run can create it.
//
// Only tokens that are plainly paths are judged: an absolute or separator-bearing token, or
// a bare name with a file extension. Anything with shell syntax in it (a substitution, a
// variable, a glob, quoted code) is left alone, since a wrong guess here refuses a good
// check, and the dry run catches what the text cannot.
func CheckGrounding(check string, g Ground) []GroundingProblem {
	if g.Dir == "" {
		return nil
	}
	dir, err := filepath.Abs(g.Dir)
	if err != nil {
		return nil
	}
	objective := strings.ToLower(filepath.ToSlash(g.Objective))
	var problems []GroundingProblem
	seen := map[string]bool{}
	for _, tok := range pathTokens(check) {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		target := tok
		if !isAbsPath(tok) {
			target = filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(tok, `\`, "/")))
		}
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if !strings.ContainsAny(tok, `/\`) && g.LookPath != nil {
			if _, err := g.LookPath(tok); err == nil {
				continue
			}
		}
		if g.Confined && !within(dir, target) {
			problems = append(problems, GroundingProblem{Path: tok, Refusal: RefusedUnwritable, Reason: tok +
				" is outside the working directory and does not exist. The commands of this run are confined to the working directory and cannot create it, so this check can never pass. Name only files inside the working directory."})
			continue
		}
		if mentioned(objective, tok) {
			continue
		}
		problems = append(problems, GroundingProblem{Path: tok, Refusal: RefusedUngrounded, Reason: tok +
			" does not exist yet and the objective does not name it. A check may read only what is already there or what the objective asks for; if the worker creates the file the check looks for, the check passes whatever the work was. Check what the objective actually requires."})
	}
	return problems
}

// within reports whether target is dir or lies under it.
func within(dir, target string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mentioned reports whether the objective names the path, by its full text or by its file name.
func mentioned(lowerObjective, tok string) bool {
	slashed := strings.ToLower(strings.ReplaceAll(tok, `\`, "/"))
	if strings.Contains(lowerObjective, slashed) {
		return true
	}
	base := slashed[strings.LastIndexByte(slashed, '/')+1:]
	return base != "" && strings.Contains(lowerObjective, base)
}

var (
	cleanPath  = regexp.MustCompile(`^[A-Za-z0-9_./\\:~@+,-]+$`)
	fileName   = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*\.[A-Za-z][A-Za-z0-9]{0,4}$`)
	driveRoot  = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	modulePath = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+/`)
	assignment = regexp.MustCompile(`^-{0,2}[A-Za-z_][A-Za-z0-9_-]*=`)
)

func isAbsPath(tok string) bool {
	return filepath.IsAbs(tok) || driveRoot.MatchString(tok) || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, `\`)
}

// pathTokens returns the tokens of a check line that are plainly paths. The tokenizer
// follows the shell only as far as quoting and the characters that end a word, so a quoted
// string is one token and code inside `python -c "..."` is never read as a file. The target
// of a redirect is the check writing its own file and is not judged.
func pathTokens(check string) []string {
	var (
		tokens   []string
		cur      strings.Builder
		quote    rune
		inTok    bool
		redirect bool
	)
	flush := func() {
		if !inTok {
			return
		}
		tok := cur.String()
		cur.Reset()
		inTok = false
		skip := redirect
		redirect = false
		if !skip && plainPath(tok) {
			tokens = append(tokens, assignmentValue(tok))
		}
	}
	for _, r := range check {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inTok = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '&' || r == '|' || r == ';' || r == '(' || r == ')':
			flush()
		case r == '>':
			flush()
			redirect = true
		case r == '<':
			flush()
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	flush()
	return filterPaths(tokens)
}

// assignmentValue is the value of a --flag=value or NAME=value token, so the path inside it
// is judged.
func assignmentValue(tok string) string {
	if loc := assignment.FindStringIndex(tok); loc != nil {
		return tok[loc[1]:]
	}
	return tok
}

// plainPath is the cheap pre-filter: a token with shell syntax in it is not a path to judge.
func plainPath(tok string) bool {
	if tok == "" || strings.Contains(tok, "://") {
		return false
	}
	if loc := assignment.FindStringIndex(tok); loc != nil {
		tok = tok[loc[1]:]
	}
	return cleanPath.MatchString(tok)
}

// filterPaths keeps the tokens that name a file: a rooted or separator-bearing path, or a
// bare file name with an extension. Flags, switches, device names, package patterns and
// module import paths are dropped.
func filterPaths(tokens []string) []string {
	var out []string
	for _, tok := range tokens {
		switch {
		case tok == "" || strings.HasPrefix(tok, "-"),
			tok == "." || tok == "..",
			strings.Contains(tok, "..."),
			strings.HasPrefix(tok, "/dev/"),
			strings.HasPrefix(tok, "~"),
			strings.EqualFold(tok, "nul"),
			modulePath.MatchString(tok):
			continue
		case strings.Contains(tok, ":") && !driveRoot.MatchString(tok):
			continue
		case len(tok) <= 3 && strings.HasPrefix(tok, "/") && !strings.ContainsAny(tok[1:], `/\`):
			continue // a cmd.exe switch such as /s or /c
		case strings.ContainsAny(tok, `/\`), fileName.MatchString(tok):
			out = append(out, tok)
		}
	}
	return out
}

// DryRunRefusal judges the verdict of running a check once on the tree before any work. It
// returns "" when the check is admitted: it ran and failed, which is what a check for work
// not yet done should do.
//
// A verdict that was not executed is admitted, with the reason left to the caller to note:
// the host could not run checks at all (no sandbox, a gate that declined), and refusing
// every plan for that would move a stall from the first verification to the planner without
// telling the operator anything new.
func DryRunRefusal(check string, v ItemVerdict) (AdmissionRefusal, string) {
	if !v.Executed {
		return "", ""
	}
	if v.Passed {
		return RefusedAlreadyPasses, "This check already passes on the tree as it is, before any work. A check that cannot fail proves nothing. Plan a check that fails now and passes only once the item is done."
	}
	if cmd, ok := MissingCommand(check, v.ExitCode, v.Output); ok {
		return RefusedMissingProgram, fmt.Sprintf("This check runs %q, which this host does not have, so it can only report the program missing. Use a program the host has.", cmd)
	}
	if line, ok := MalformedCheck(v.Output); ok {
		return RefusedMalformed, fmt.Sprintf("The shell could not parse this check (%s). Check the quoting and syntax for the host's shell.", line)
	}
	return "", ""
}

var (
	shSyntax  = regexp.MustCompile(`(?mi)^(?:[^\n:]+: )+(?:syntax error|unexpected EOF|unexpected end of file|unterminated quoted string)[^\n]*$`)
	cmdSyntax = regexp.MustCompile(`(?mi)^(?:The syntax of the command is incorrect\.|The filename, directory name, or volume label syntax is incorrect\.|.* was unexpected at this time\.)\s*$`)
	psSyntax  = regexp.MustCompile(`(?i)ParserError|ParseException|The string is missing the terminator|Missing closing '[)}\]]'`)
)

// MalformedCheck reports the shell's own complaint when it rejected a check's text rather
// than running it. The patterns are the shells' wording for a parse failure, anchored so the
// output of a program that merely mentions a syntax error does not read as one.
func MalformedCheck(output string) (string, bool) {
	for _, re := range []*regexp.Regexp{cmdSyntax, shSyntax, psSyntax} {
		if m := re.FindString(output); m != "" {
			return strings.TrimSpace(m), true
		}
	}
	return "", false
}
