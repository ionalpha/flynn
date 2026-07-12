package notices

import (
	"fmt"
	"io"
)

// Pending returns the notices in f that apply to the running Flynn version and have not
// already been said.
//
// "Already said" is where the two severities part company. A release notice or a
// deprecation is shown once and then remembered, because repeating it is nagging. A
// security notice is shown every single time for as long as it applies, and is only ever
// silenced by the user moving to a version that fixes it. A user who is still exposed
// still needs to be told, and a channel that let a vulnerability scroll past once and then
// went quiet about it would be worse than no channel: it would look like diligence.
func Pending(f Feed, flynnVersion string, t Trust) []Notice {
	var out []Notice
	for _, n := range f.Notices {
		if !Applies(n, flynnVersion) {
			continue
		}
		if n.Severity != Security && t.hasShown(n.ID) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// Render writes the pending notices to w, security first, and reports whether it wrote
// anything.
//
// The text has already been through Sanitize at decode, so what reaches the terminal here
// carries no escape sequence and cannot repaint what was printed above it. A URL is
// printed as text and is never opened. Nothing here asks a question, blocks, or changes
// what the command was going to do: the user asked Flynn to do something, and a notice
// interrupting that would be a channel that acts.
func Render(w io.Writer, ns []Notice, stale bool) bool {
	wrote := false
	for _, sev := range []Severity{Security, Deprecation, Info} {
		for _, n := range ns {
			if n.Severity != sev {
				continue
			}
			_, _ = fmt.Fprintf(w, "%s %s\n", label(sev), n.Summary)
			if n.URL != "" {
				_, _ = fmt.Fprintf(w, "         %s\n", n.URL)
			}
			wrote = true
		}
	}
	// Staleness is reported next to the notices, never instead of them, and it is
	// reported at all precisely because an attacker who can block our origin would
	// otherwise get silence for free and silence reads as all-clear.
	if stale {
		_, _ = fmt.Fprintln(w, "notice:  the advisory feed has not been refreshed recently; this may not be the current list")
		wrote = true
	}
	return wrote
}

// label is the prefix a severity is announced with. Security is spelled out because it is
// the one the user must not skim past.
func label(s Severity) string {
	switch s {
	case Security:
		return "SECURITY:"
	case Deprecation:
		return "notice: "
	default:
		return "notice: "
	}
}

// Cached loads and re-verifies the cached feed. Every run re-checks the signature, the
// origin, and the rollback rule against the stored trust state, so a cache file that was
// edited on disk since it was written is discarded here rather than believed. There is no
// fast path that skips this, because the fast path is exactly where a local attacker would
// put words in our mouth.
func Cached(store *Store, ring *Keyring) (Feed, Trust, bool) {
	tr, err := store.LoadTrust()
	if err != nil {
		return Feed{}, Trust{}, false
	}
	doc, err := store.LoadFeed()
	if err != nil || len(doc) == 0 {
		return Feed{}, tr, false
	}
	f, err := Verify(doc, ring)
	if err != nil {
		return Feed{}, tr, false
	}
	if f.Version < tr.Version {
		// The cached document is older than the newest feed this client has trusted,
		// which means the file was rolled back underneath us. Refuse it.
		return Feed{}, tr, false
	}
	return f, tr, true
}
