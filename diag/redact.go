package diag

import (
	"strings"

	"github.com/ionalpha/flynn/secret"
)

// sensitiveFlags are flag-name fragments whose value is a credential, redacted in
// both --name=value and --name value form.
var sensitiveFlags = []string{"token", "key", "secret", "password", "passwd", "credential", "auth"}

// freeTextCommands take user text: an objective can hold anything, and `auth set`
// carries a credential, so nothing after either is recorded.
var freeTextCommands = map[string]bool{"goal": true, "auth": true}

// credentialPrefixes are the shapes of a bare credential with no flag naming it.
// Over-redacting costs a reader nothing; a leaked API key costs a rotation.
var credentialPrefixes = []string{"sk-", "sk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "xoxb-", "xoxp-", "AKIA", "AIza", "hf_"}

// RedactArgs returns args with credentials and free user text replaced by
// secret.Redacted, safe for a manifest attached to a bug report. It redacts
// credential-named flag values, everything after a free-text subcommand and bare
// arguments with a credential prefix. Program path, flag names, subcommands and
// ordinary values stay, so the line still says what was profiled.
//
// A subcommand is recognized by name anywhere among bare arguments, because this
// package does not know which flags take a value, and a positional guess could
// leave an objective in the clear.
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
			// Which binary ran is part of the evidence.
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
				// Without the flag set a boolean flag is indistinguishable, so the next token
				// is redacted only when it does not look like a flag.
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

// redacted renders a withheld value through the agent's one redactor.
func redacted() string { return secret.New("").String() }

// sensitiveFlagName reports whether a flag name, dashes stripped, names a credential.
func sensitiveFlagName(flag string) bool {
	name := strings.ToLower(strings.TrimLeft(flag, "-"))
	for _, frag := range sensitiveFlags {
		if strings.Contains(name, frag) {
			return true
		}
	}
	return false
}

// looksLikeCredential reports whether arg has a known credential prefix and enough
// after it to be one.
func looksLikeCredential(arg string) bool {
	for _, p := range credentialPrefixes {
		if len(arg) > len(p)+8 && strings.HasPrefix(arg, p) {
			return true
		}
	}
	return false
}
