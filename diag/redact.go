package diag

import (
	"strings"

	"github.com/ionalpha/flynn/secret"
)

// sensitiveFlags are the flag-name fragments whose value is a credential. A flag
// whose name contains any of these has its value replaced, whether it was passed
// as --name=value or as --name value.
var sensitiveFlags = []string{"token", "key", "secret", "password", "passwd", "credential", "auth"}

// freeTextCommands are the subcommands whose arguments are user text rather than
// identifiers. A goal's objective can contain anything the user knows, and
// `auth set` carries a credential, so nothing after either is recorded.
var freeTextCommands = map[string]bool{"goal": true, "auth": true}

// credentialPrefixes are the shapes a bare credential takes when it reaches a
// command line without a flag naming it. They are cheap to spot and cheap to be
// wrong about: over-redacting an argument costs a reader nothing, and writing a
// live API key into a file an operator will mail around costs them a rotation.
var credentialPrefixes = []string{"sk-", "sk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "xoxb-", "xoxp-", "AKIA", "AIza", "hf_"}

// RedactArgs returns args with every credential and every piece of free user text
// replaced by secret.Redacted. The result is safe to write into a bundle manifest
// that an operator will attach to a bug report.
//
// It redacts three things: the value of any flag whose name looks like a
// credential, every argument following a subcommand that takes free text (a goal's
// objective, an `auth set` key), and any bare argument carrying a recognizable
// credential prefix. The program path, flag names, subcommands, and ordinary
// values are preserved, because a manifest whose command line is entirely redacted
// tells a reader nothing about what was being profiled.
//
// Redaction reuses secret.Text rather than formatting a placeholder here, so the
// agent has exactly one rendering of a withheld value.
//
// A subcommand is recognized by name wherever it appears among the bare arguments
// rather than by position. flag's own parse rules cannot be reproduced here (this
// package does not know which of flynn's flags take a value), and guessing wrong
// about position would silently leave an objective in the clear.
func RedactArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}

	out := make([]string, 0, len(args))
	freeText := false   // a free-text subcommand has been seen; everything after it is user text
	redactNext := false // the previous token was a sensitive flag awaiting its value

	for i, arg := range args {
		switch {
		case i == 0:
			// The program path. Which binary ran is part of the evidence.
			out = append(out, arg)

		case redactNext:
			out = append(out, redacted())
			redactNext = false

		case freeText:
			out = append(out, redacted())

		case strings.HasPrefix(arg, "-"):
			name, _, inline := strings.Cut(arg, "=")
			switch {
			case !sensitiveFlagName(name):
				out = append(out, arg)
			case inline:
				out = append(out, name+"="+redacted())
			default:
				// The value is the next token, unless this is a boolean flag and the next
				// token is another flag. Telling those apart needs the flag set, which
				// this package does not have, so the next token is redacted only when it
				// does not itself look like a flag.
				out = append(out, arg)
				redactNext = i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
			}

		case looksLikeCredential(arg):
			out = append(out, redacted())

		default:
			if freeTextCommands[arg] {
				freeText = true
			}
			out = append(out, arg)
		}
	}
	return out
}

// redacted renders a withheld value through the agent's single redactor.
func redacted() string { return secret.New("").String() }

// sensitiveFlagName reports whether a flag's name, with its leading dashes
// stripped, names a credential.
func sensitiveFlagName(flag string) bool {
	name := strings.ToLower(strings.TrimLeft(flag, "-"))
	for _, frag := range sensitiveFlags {
		if strings.Contains(name, frag) {
			return true
		}
	}
	return false
}

// looksLikeCredential reports whether a bare argument carries the prefix of a
// well-known credential format, with enough trailing material to be a real one.
func looksLikeCredential(arg string) bool {
	for _, p := range credentialPrefixes {
		if len(arg) > len(p)+8 && strings.HasPrefix(arg, p) {
			return true
		}
	}
	return false
}
