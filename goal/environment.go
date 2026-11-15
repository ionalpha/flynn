package goal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	exitCmdNotFound = 9009
	exitShNotFound  = 127

	// EnvironmentRepeatLimit is two, because after the first the agent may install the
	// program and the second shows it did not.
	EnvironmentRepeatLimit = 2
)

var (
	// cmdNotFound is anchored to a line start, because a test quoting the phrase mid-line
	// has run.
	cmdNotFound = regexp.MustCompile(`(?m)^'([^']+)' is not recognized as an internal or external command`)
	shNotFound  = regexp.MustCompile(`(?m)([^\s:]+): (?:command )?not found\s*$`)
	// psNotFound matches PowerShell's own phrase, because it exits 1 and the text is the
	// only evidence.
	psNotFound = regexp.MustCompile(`The term '([^']+)' is not recognized as the name of a cmdlet`)
)

// MissingCommand reports a program the host could not find. cmd's sentence counts at any
// non-zero exit, because cmd exits 1 for a missing program in a redirect or chain, and
// 9009 counts without text. A shell's "not found" counts only at 127, because it is common
// in output of a check that ran.
func MissingCommand(check string, exitCode int, output string) (string, bool) {
	if exitCode == 0 {
		return "", false
	}
	if m := cmdNotFound.FindStringSubmatch(output); m != nil {
		return m[1], true
	}
	switch exitCode {
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

func firstSubmatch(re *regexp.Regexp, output, check string) string {
	if m := re.FindStringSubmatch(output); m != nil {
		return m[1]
	}
	if words := commandWords(check); len(words) > 0 {
		return words[0]
	}
	return strings.TrimSpace(check)
}

// classifyUnrunnable strips execution provenance from a missing-program failure, so it is
// not counted toward non-convergence.
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

// StalledForEnvironment reports whether one program reached [EnvironmentRepeatLimit].
func (s Status) StalledForEnvironment() bool {
	return s.MissingCommand != "" && s.MissingRepeat >= EnvironmentRepeatLimit
}

// EnvironmentReason names the program, the one fact an operator needs.
func (s Status) EnvironmentReason() string {
	return fmt.Sprintf("the check cannot run on this host: the program %q was not found on %d consecutive verifications. "+
		"The work was not judged. Rewrite the item's check with a program this host has, or install %s.",
		s.MissingCommand, s.MissingRepeat, s.MissingCommand)
}

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

// standInExts are checked on every platform, because a stand-in works whichever shell
// reads the check.
var standInExts = []string{"", ".bat", ".cmd", ".com", ".exe", ".ps1"}

// ShadowedCommands finds a file in dir standing in for a check's program, such as test.bat
// for `test`. That is the agent editing its verifier and is refused as tampering. Only dir
// is searched, where cmd.exe looks first; a path-qualified command is left alone.
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

// NamedCommands lets a planner catch a check that calls one of the run's tools, which no
// shell can start. Each name appears once, in order.
func NamedCommands(check string, names []string) []string {
	var found []string
	for _, word := range commandWords(check) {
		if slices.Contains(names, word) && !slices.Contains(found, word) {
			found = append(found, word)
		}
	}
	return found
}

var commandSeparators = strings.NewReplacer("&&", "\n", "||", "\n", "|", "\n", ";", "\n", "$(", "\n", "(", "\n", "`", "\n")

func startsWord(w string) bool {
	if w == "" {
		return false
	}
	c := w[0]
	return c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// commandWords never returns an argument, so a file named like one cannot read as a
// stand-in.
func commandWords(check string) []string {
	var words []string
	for _, segment := range strings.Split(commandSeparators.Replace(check), "\n") {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		word := strings.Trim(fields[0], `"'`)
		// Leading punctuation is an unparsed construct's tail; "=" is an assignment.
		if !startsWord(word) || strings.Contains(word, "=") {
			continue
		}
		words = append(words, word)
	}
	return words
}
