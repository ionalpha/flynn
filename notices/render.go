package notices

import (
	"fmt"
	"io"
)

// Pending returns the notices in f that apply to flynnVersion and still need showing.
// Info and deprecation notices are shown once. A security notice is shown on every run
// until the user is on a fixed version, because a user who is still exposed still needs
// telling.
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

// Render writes ns to w, security first, and reports whether it wrote anything. The
// text was sanitized at decode. A URL is printed and never opened, and nothing here
// prompts or blocks, so a notice cannot change what the command does.
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
	// Reported beside the notices, so blocking the origin does not read as all-clear.
	if stale {
		_, _ = fmt.Fprintln(w, "notice:  the advisory feed has not been refreshed recently; this may not be the current list")
		wrote = true
	}
	return wrote
}

// label spells out SECURITY so the user does not skim past it.
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

// Cached loads the cached feed and re-checks its signature, origin and rollback rule
// on every call, so a cache file edited on disk is discarded. There is no fast path,
// because a local attacker would use it to forge a notice.
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
		// The cache file was rolled back on disk.
		return Feed{}, tr, false
	}
	return f, tr, true
}
