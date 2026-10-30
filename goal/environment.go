package goal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A check can fail in three different ways, and only one of them is the work being wrong.
// The program can run and exit non-zero, which is a run being told its work is not done and
// is what non-convergence (converge.go) counts. The program can be one the host does not
// have, so nothing was checked at all; that is the environment, and repeating the build
// step cannot change it. Or a file in the workspace can stand in for the program the check
// names, so what ran was not the check the record holds. This file tells the first from the
// other two.

const (
	// exitCmdNotFound is what cmd.exe exits with when it cannot find the program it was
	// asked to run.
	exitCmdNotFound = 9009
	// exitShNotFound is what a POSIX shell exits with for the same condition.
	exitShNotFound = 127

	// EnvironmentRepeatLimit is how many consecutive verifications may find the same
	// program missing before the goal stops. The first tells the agent, which may be able
	// to install the program; the second says it did not.
	EnvironmentRepeatLimit = 2
)

var (
	cmdNotFound = regexp.MustCompile(`'([^']+)' is not recognized as an internal or external command`)
	shNotFound  = regexp.MustCompile(`(?m)([^\s:]+): (?:command )?not found\s*$`)
	// psNotFound is PowerShell's wording, which carries no distinctive exit code (it is 1),
	// so the text is the only evidence and the phrase has to be PowerShell's own.
	psNotFound = regexp.MustCompile(`The term '([^']+)' is not recognized as the name of a cmdlet`)
)

// MissingCommand reports the program a check named that the host could not find, and false
// when the check ran, or failed for another reason.
//
// The exit code decides before the text does. cmd.exe exits 9009 and a POSIX shell 127 for
// a program they cannot find, and those two are trusted on the code alone; the output only
// names the program. A bare "not found" in the output of a check that exited 1 is not
// evidence, because a test run that prints "route: not found" has run its check and been
// refused by it. PowerShell exits 1, so for it the text is the evidence.
func MissingCommand(check string, exitCode int, output string) (string, bool) {
	switch exitCode {
	case 0:
		return "", false
	case exitCmdNotFound:
		return firstSubmatch(cmdNotFound, output, check), true
	case exitShNotFound:
		return firstSubmatch(shNotFound, output, check), true
	}
	if m := psNotFound.FindStringSubmatch(output); m != nil {
		return m[1], true
	}
	return "", false
}

// firstSubmatch is the program the output names, or the check's own first word when the
// shell did not say.
func firstSubmatch(re *regexp.Regexp, output, check string) string {
	if m := re.FindStringSubmatch(output); m != nil {
		return m[1]
	}
	if words := commandWords(check); len(words) > 0 {
		return words[0]
	}
	return strings.TrimSpace(check)
}

// classifyUnrunnable turns an executed verdict that failed because the host lacks the
// check's program into an unexecuted one saying so. Nothing was verified, so the verdict
// must not carry execution provenance: it would otherwise be counted as the work failing
// its check, and the goal would stop as not converging on a check that never ran.
func classifyUnrunnable(check string, v ItemVerdict) ItemVerdict {
	if !v.Executed || v.Passed {
		return v
	}
	cmd, ok := MissingCommand(check, v.ExitCode, v.Output)
	if !ok {
		return v
	}
	v.Executed = false
	v.Missing = cmd
	v.Detail = fmt.Sprintf("the check could not run: the program %q does not exist on this host (exit %d), so nothing was checked. "+
		"This is the environment and not your work failing. Do not create a file named %s to stand in for it: that is tampering with the check, and the check will refuse it.",
		cmd, v.ExitCode, cmd)
	return v
}

// StalledForEnvironment reports whether the same program has been found missing often
// enough that another build step will not change it.
func (s Status) StalledForEnvironment() bool {
	return s.MissingCommand != "" && s.MissingRepeat >= EnvironmentRepeatLimit
}

// EnvironmentReason is the stall message for a check whose program the host does not have.
// It names the program, because that is the one fact an operator needs to fix it, and says
// the work was not judged.
func (s Status) EnvironmentReason() string {
	return fmt.Sprintf("the check cannot run on this host: the program %q was not found on %d consecutive verifications. "+
		"The work was not judged. Rewrite the item's check with a program this host has, or install %s.",
		s.MissingCommand, s.MissingRepeat, s.MissingCommand)
}

// observeMissing folds one verdict into the missing-program count: the same program again
// extends it, a different one restarts it, and any verdict that is not a missing program
// ends it.
func (s *Status) observeMissing(missing string) {
	switch missing {
	case "":
		s.MissingCommand, s.MissingRepeat = "", 0
	case s.MissingCommand:
		s.MissingRepeat++
	default:
		s.MissingCommand, s.MissingRepeat = missing, 1
	}
}

// standInExts are the suffixes a shell resolves a bare program name through. They are
// checked on every platform: a file written to answer to the check's command is a stand-in
// whichever shell happens to read the check.
var standInExts = []string{"", ".bat", ".cmd", ".com", ".exe", ".ps1"}

// ShadowedCommands returns the programs a check names that a file directly in dir stands
// in for, such as a test.bat beside a check that runs `test`.
//
// A check is the record's, written before the work and judged after it, so a file that
// answers to the check's own command makes the check measure what the agent wrote and not
// what the objective asked for. That is the agent editing its verifier, and it is refused
// as tampering and not accepted as a fix. Only the directory itself is searched, which is
// where cmd.exe looks first; a path-qualified command is the author naming the file, and is
// left alone.
func ShadowedCommands(check, dir string) []string {
	if dir == "" {
		return nil
	}
	var shadowed []string
	seen := map[string]bool{}
	for _, word := range commandWords(check) {
		if seen[word] || strings.ContainsAny(word, `/\`) || strings.HasPrefix(word, ".") {
			continue
		}
		seen[word] = true
		for _, ext := range standInExts {
			if info, err := os.Stat(filepath.Join(dir, word+ext)); err == nil && info.Mode().IsRegular() {
				shadowed = append(shadowed, word)
				break
			}
		}
	}
	return shadowed
}

// commandSeparators split a check into the segments each of which starts with a program.
var commandSeparators = strings.NewReplacer("&&", "\n", "||", "\n", "|", "\n", ";", "\n", "$(", "\n", "(", "\n", "`", "\n")

func startsWord(w string) bool {
	if w == "" {
		return false
	}
	c := w[0]
	return c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// commandWords is the first word of each command in a check line, unquoted, in order. It
// reads the shell's grammar only as far as the separators that start a new command; a
// word that is an argument is never returned, so a file named like an argument cannot
// read as a stand-in.
func commandWords(check string) []string {
	var words []string
	for _, segment := range strings.Split(commandSeparators.Replace(check), "\n") {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		word := strings.Trim(fields[0], `"'`)
		// A leading punctuation mark is the tail of a construct this does not parse (the
		// ")" closing a substitution), and a "=" makes the word an assignment.
		if !startsWord(word) || strings.Contains(word, "=") {
			continue
		}
		words = append(words, word)
	}
	return words
}
