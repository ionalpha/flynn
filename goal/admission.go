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

// AdmissionRefusal values are recorded on the spine, so they stay stable. Admission runs
// before work, so a wrong check goes back to the planner instead of proving an item.
type AdmissionRefusal string

const (
	// RefusedUngrounded is a missing path the objective never names, which only the worker
	// creating it could satisfy.
	RefusedUngrounded AdmissionRefusal = "ungrounded"
	// RefusedUnwritable is a missing path outside the working directory, which confined
	// commands cannot create.
	RefusedUnwritable AdmissionRefusal = "unwritable"
	// RefusedMalformed means the shell could not parse the check.
	RefusedMalformed AdmissionRefusal = "malformed"
	// RefusedMissingProgram means the check runs a program the host does not have.
	RefusedMissingProgram AdmissionRefusal = "missing_program"
	// RefusedAlreadyPasses means a later pass would distinguish nothing.
	RefusedAlreadyPasses AdmissionRefusal = "already_passes"
)

// AdmissionVerdict names its check by content address ([ItemID]).
type AdmissionVerdict struct {
	Item     string
	Admitted bool
	Refusal  AdmissionRefusal
	// Reason goes to the planner. On an admitted verdict it notes what was not checked.
	Reason string
	// DryRun false on an admitted verdict means it was admitted on its text alone.
	DryRun bool
}

// AdmissionRecorder is apart from [Evidence] so hosts implementing Evidence need nothing
// new. evidence.SpineEvidence implements both.
type AdmissionRecorder interface {
	RecordAdmission(ctx context.Context, r resource.Resource, v AdmissionVerdict) error
}

// Ground is what grounding a check is judged against.
type Ground struct {
	// Dir empty disables grounding, because a host sandbox has no directory the run chose.
	Dir string
	// Confined reports that commands cannot write outside Dir.
	Confined bool
	// Objective holds the objective and stop condition.
	Objective string
	// LookPath resolves a bare name on the program search path.
	LookPath func(string) (string, error)
}

// GroundingProblem is one path in a check that admission refuses.
type GroundingProblem struct {
	Path    string
	Refusal AdmissionRefusal
	Reason  string
}

// CheckGrounding requires each read path to exist or be named by the objective, so a
// planner cannot invent an artifact the worker then creates. Only plain paths are judged;
// shell syntax is left alone, because a wrong guess refuses a good check and the dry run
// catches the rest.
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

func within(dir, target string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

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

// pathTokens keeps a quoted string as one token, so code in `python -c "..."` is never
// read as a file. A redirect target is the check's own output and is skipped.
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

func assignmentValue(tok string) string {
	if loc := assignment.FindStringIndex(tok); loc != nil {
		return tok[loc[1]:]
	}
	return tok
}

func plainPath(tok string) bool {
	if tok == "" || strings.Contains(tok, "://") {
		return false
	}
	if loc := assignment.FindStringIndex(tok); loc != nil {
		tok = tok[loc[1]:]
	}
	return cleanPath.MatchString(tok)
}

// filterPaths drops flags, switches, device names, package patterns and module paths.
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

// DryRunRefusal returns "" for a check that ran and failed, as one for undone work should.
// An unexecuted verdict is admitted, because refusing every plan on a host that cannot run
// checks only moves the stall to the planner.
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

// MalformedCheck returns the shell's parse complaint. The patterns are anchored so output
// that mentions a syntax error does not read as one.
func MalformedCheck(output string) (string, bool) {
	for _, re := range []*regexp.Regexp{cmdSyntax, shSyntax, psSyntax} {
		if m := re.FindString(output); m != "" {
			return strings.TrimSpace(m), true
		}
	}
	return "", false
}
