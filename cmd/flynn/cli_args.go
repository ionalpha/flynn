package main

import (
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
)

// The checks here keep one promise: a word the operator typed is either read or refused.
// The flag package stops at the first positional and a hand-written handler reads the
// arguments it indexes, so without them a flag is dropped or taken for a run id.

// runIDArgs reads the one run id a command takes, with -v on either side of it.
func runIDArgs(path string, args []string, verbose bool) (string, bool, error) {
	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&verbose, "v", verbose, "")
	fs.BoolVar(&verbose, "verbose", verbose, "")
	bare, err := parseAroundArgs(fs, args)
	if err != nil {
		return "", false, asUsage(path, err)
	}
	if len(bare) != 1 {
		return "", false, usageErr(path)
	}
	return bare[0], verbose, nil
}

// parseModelArgs is [parseModelFlags] for a subcommand that takes positionals: its flags
// are read on either side of them.
func parseModelArgs(fs *flag.FlagSet, args []string) (bare []string, done bool, err error) {
	bare, err = parseAroundArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return nil, true, nil
	}
	return bare, err != nil, err
}

// refuseFlagWords refuses a flag left among args once a handler has taken the ones it
// knows, so it is never read as a name or a search word.
func refuseFlagWords(path string, args []string) error {
	for _, a := range args {
		if _, isFlag := flagName(a); isFlag {
			return usageErrf(path, "flag provided but not defined: %s", a)
		}
	}
	return nil
}

// refuseUnreadRunFlags refuses a run control written before a command that does not read
// it, so an operator never believes a ceiling or a gate is in force when it is not. c is
// nil for an interactive session, whose one run control is --require-approval. A command's
// runFlags are the list of what it reads.
func refuseUnreadRunFlags(c *cliCommand, flags *flag.FlagSet) error {
	if flags == nil || (c != nil && c.name == "help") {
		return nil
	}
	reads := []string{"require-approval"}
	what := "an interactive session"
	if c != nil {
		reads, what = c.runFlags, "flynn "+c.name
	}
	var err error
	flags.Visit(func(f *flag.Flag) {
		if err != nil || !slices.Contains(runControlFlags(), f.Name) || slices.Contains(reads, f.Name) {
			return
		}
		if c != nil && slices.ContainsFunc(c.flags, func(own cliFlag) bool { return own.name == f.Name }) {
			err = usageErrf("flags", "--%s before `%s` is not read by it; the command has its own, so put it after: %s ... --%s", f.Name, what, what, f.Name)
			return
		}
		var readers []string
		for _, other := range cliCommands() {
			if slices.Contains(other.runFlags, f.Name) {
				readers = append(readers, "`flynn "+other.name+"`")
			}
		}
		if f.Name == "require-approval" {
			readers = append(readers, "an interactive session")
		}
		err = usageErrf("flags", "--%s applies to %s, and %s would ignore it", f.Name, strings.Join(readers, ", "), what)
	})
	return err
}

// runControlFlags are the root flags that govern a run, as `flynn help flags` groups them.
func runControlFlags() []string {
	for _, g := range rootFlagGroups {
		if g.title == "Run controls" {
			return g.names
		}
	}
	return nil
}

// refuseUnparsedArgs refuses what a command with no flags of its own would not read. Its
// handler indexes its arguments, so a flag becomes a name or is skipped: `db reset
// --dry-run` would reset. The registry says which commands those are and how many words
// each takes. args are the words after the command.
func refuseUnparsedArgs(c *cliCommand, args []string) error {
	path := c.name
	if len(args) > 0 {
		if s := findCommand(c.subs, args[0]); s != nil {
			c, path, args = s, path+" "+s.name, args[1:]
		} else if len(c.subs) > 0 {
			return nil // the handler refuses a subcommand it does not have
		}
	} else if len(c.subs) > 0 {
		return nil
	}
	if len(c.flags) > 0 || len(c.runFlags) > 0 {
		return nil // the command parses its own arguments
	}
	words, open := usageArity(c.usage)
	for i, a := range args {
		// Free text may mention a flag, so only its first word is checked.
		if _, isFlag := flagName(a); isFlag && (!open || i == 0) {
			return usageErrf(path, "flynn %s does not take %s; root flags go before the command", path, a)
		}
	}
	if !open && len(args) > words {
		return usageErrf(path, "unexpected argument %q", args[words])
	}
	return nil
}

// usageArity reads from a synopsis how many words a command takes at most, and whether it
// takes free text or a list, which has no maximum.
func usageArity(usage []string) (words int, open bool) {
	for _, u := range usage {
		if strings.Contains(u, "...") || strings.Contains(u, `"`) || strings.Contains(u, "[flags]") || strings.Contains(u, "json") {
			return 0, true
		}
		words = max(words, strings.Count(u, "<"))
	}
	return words, false
}
