package notices

import (
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/ionalpha/flynn/fault"
)

// Origin names this feed and its format version. It is inside the signed payload, so a
// document signed for some other purpose by the same key can never be read as a notice
// feed, and a future format change is a new origin rather than a silent reinterpretation
// of the same bytes.
const Origin = "flynn/notices/v1"

// Limits on what a feed may contain. They bound the memory a hostile origin can make a
// client spend, and they bound the screen: a feed cannot bury a real advisory under a
// thousand entries, and one notice cannot scroll the terminal away.
const (
	MaxNotices    = 32
	MaxFloors     = 16
	MaxSummary    = 200
	MaxDetail     = 2000
	MaxIDLen      = 64
	MaxURLLen     = 200
	MaxVersionLen = 32
	// MaxFeedBytes caps the signed document itself. A notice feed is a few kilobytes;
	// anything approaching this is not a feed we published.
	MaxFeedBytes = 256 << 10
)

// Severity says how loudly a notice is shown and, more importantly, what it means. Only
// these three exist: an unknown severity is a decoding failure, not a fourth kind
// rendered as a curiosity.
type Severity string

const (
	// Security is a vulnerability in Flynn itself. It is shown on every run until the
	// user is on a version that fixes it; it cannot be dismissed away.
	Security Severity = "security"
	// Deprecation is behaviour that will be removed or changed. It is shown once.
	Deprecation Severity = "deprecation"
	// Info is a release notice or announcement. It is shown once, quietly.
	Info Severity = "info"
)

// Valid reports whether s is a severity this client understands.
func (s Severity) Valid() bool {
	return s == Security || s == Deprecation || s == Info
}

// Failure codes. They are stable dotted identifiers so a refusal names the check that
// failed rather than just "invalid feed", which matters when the thing being refused is
// a security advisory and the user needs to know whether they are being attacked or
// whether we shipped a bad file.
const (
	CodeDecode        = "notices.decode"
	CodeEncode        = "notices.encode"
	CodeOrigin        = "notices.wrong_origin"
	CodeSignerKey     = "notices.bad_key"
	CodeUnknownKey    = "notices.unknown_key"
	CodeContentType   = "notices.bad_content_type"
	CodeSignature     = "notices.signature_invalid"
	CodeRollback      = "notices.rollback"
	CodeMalformed     = "notices.malformed"
	CodeTooLarge      = "notices.too_large"
	CodeFetch         = "notices.fetch_failed"
	CodeStateCorrupt  = "notices.state_corrupt"
	CodeStateNotSaved = "notices.state_not_saved"
)

// Notice is one thing we have to tell the user.
type Notice struct {
	// ID is stable and unique within a feed. It is what a client remembers when it has
	// already shown a notice, so reusing an ID for different content silently suppresses
	// the new content.
	ID string
	// Severity selects how it is shown and whether it can be dismissed.
	Severity Severity
	// Summary is the one line the user sees. Sanitized before it reaches a terminal.
	Summary string
	// Detail is the optional longer text shown by `flynn notices`. Sanitized too.
	Detail string
	// URL points at the full writeup. It is printed as text and never opened: a feed
	// that could open a browser would be a feed that could act.
	URL string
	// AffectedFrom is the first Flynn version the notice applies to. Empty means every
	// version from the beginning.
	AffectedFrom string
	// FixedIn is the first Flynn version that resolves it, and the notice does not apply
	// from there on. Empty means no version fixes it (an announcement, or an advisory
	// with no fix released yet), so it applies to every version at or above
	// AffectedFrom.
	FixedIn string
}

// Floor is a minimum safe version for a local inference runtime, carried in the feed so a
// parser flaw disclosed after a release can still raise the gate on an installed Flynn.
//
// This is the one thing in a feed that is not inert text, and it is allowed for exactly
// one reason: it can only ever tighten. The client takes the higher of its compiled-in
// floor and this one (see inference.Raise), and no path exists to lower a floor. An
// attacker holding the origin and the signing key can therefore only make Flynn refuse to
// run a runtime, never make it run a vulnerable one. A denial of service is loud and
// recoverable; a relaxed gate on a parser with a live remote-code-execution bug is not.
type Floor struct {
	// Runtime is the runtime this floor applies to ("llama.cpp", "ollama", "vllm"). A name
	// Flynn does not drive is ignored, not invented.
	Runtime string
	// MinVersion is the oldest version considered safe, in that runtime's own version
	// shape (a build number for llama.cpp, a semver for the others).
	MinVersion string
	// AdvisoryID names what the floor is for, so a refusal from a floor this build was not
	// compiled with still tells the user what to go and read.
	AdvisoryID string
}

// Feed is the signed document: every notice we currently have, plus the two fields that
// make replaying or suppressing it detectable.
type Feed struct {
	// Version increases with every publication and never decreases. It is the whole
	// anti-rollback mechanism: a client refuses a feed older than the newest one it has
	// already trusted, so an old feed cannot be replayed over a new one.
	Version uint64
	// Issued is when this feed was signed.
	Issued time.Time
	// Expires is when this feed stops being believable. We re-sign on a schedule even
	// when nothing changed, so a client that sees an expired feed has learned something
	// real: it is not being shown the current one.
	Expires time.Time
	// Notices is the full current set, not a delta. A feed is the complete state, so a
	// client that missed ten publications is not missing ten notices.
	Notices []Notice
	// Floors are the runtime version gates in force. They only ever tighten (see Floor).
	Floors []Floor
}

// canonicalEnc and canonicalDec are the shared CBOR modes the signed payload is defined
// by: RFC 8949 core deterministic encoding out, and a strict decoder in that rejects
// duplicate map keys and indefinite-length items, the two ambiguities that would let two
// different byte strings claim to be the same feed. This is the same pairing chain/ uses,
// and for the same reason: the bytes that are signed must have exactly one meaning.
var (
	canonicalEnc cbor.EncMode
	canonicalDec cbor.DecMode
)

func init() {
	enc, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("notices: build canonical CBOR encoder: " + err.Error())
	}
	canonicalEnc = enc

	dec, err := cbor.DecOptions{
		DupMapKey:   cbor.DupMapKeyEnforcedAPF,
		IndefLength: cbor.IndefLengthForbidden,
	}.DecMode()
	if err != nil {
		panic("notices: build canonical CBOR decoder: " + err.Error())
	}
	canonicalDec = dec
}

// wireFeed and wireNotice are the on-the-wire shape. They are separate from Feed and
// Notice so the field names in the signed document are fixed by this file and cannot
// drift when someone renames a Go field, and so times cross the wire as unambiguous Unix
// seconds rather than a formatted string with a timezone in it.
type wireFeed struct {
	Origin  string       `cbor:"origin"`
	Version uint64       `cbor:"version"`
	Issued  int64        `cbor:"issued"`
	Expires int64        `cbor:"expires"`
	Notices []wireNotice `cbor:"notices"`
	Floors  []wireFloor  `cbor:"floors"`
}

type wireFloor struct {
	Runtime    string `cbor:"runtime"`
	MinVersion string `cbor:"min_version"`
	AdvisoryID string `cbor:"advisory_id"`
}

type wireNotice struct {
	ID           string `cbor:"id"`
	Severity     string `cbor:"severity"`
	Summary      string `cbor:"summary"`
	Detail       string `cbor:"detail"`
	URL          string `cbor:"url"`
	AffectedFrom string `cbor:"affected_from"`
	FixedIn      string `cbor:"fixed_in"`
}

// payload encodes f as the deterministic CBOR bytes that get signed.
func payload(f Feed) ([]byte, error) {
	w := wireFeed{
		Origin:  Origin,
		Version: f.Version,
		Issued:  f.Issued.Unix(),
		Expires: f.Expires.Unix(),
		Notices: make([]wireNotice, 0, len(f.Notices)),
		Floors:  make([]wireFloor, 0, len(f.Floors)),
	}
	for _, n := range f.Notices {
		w.Notices = append(w.Notices, wireNotice{
			ID:           n.ID,
			Severity:     string(n.Severity),
			Summary:      n.Summary,
			Detail:       n.Detail,
			URL:          n.URL,
			AffectedFrom: n.AffectedFrom,
			FixedIn:      n.FixedIn,
		})
	}
	for _, fl := range f.Floors {
		w.Floors = append(w.Floors, wireFloor(fl))
	}
	b, err := canonicalEnc.Marshal(w)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeEncode, err)
	}
	return b, nil
}

// decodePayload decodes and structurally validates a signed payload. It runs only on
// bytes whose signature already verified, but it is still written as though the bytes
// were hostile: our own signing key could be misused, or a future publisher could make a
// mistake, and neither should be able to hand a client a feed it will mis-render.
//
// Every text field is sanitized here rather than at the print site, so there is exactly
// one place where feed text becomes something a terminal will see, and no future caller
// can forget to go through it.
func decodePayload(b []byte) (Feed, error) {
	var w wireFeed
	if err := canonicalDec.Unmarshal(b, &w); err != nil {
		return Feed{}, fault.Wrap(fault.Terminal, CodeDecode, err)
	}
	if w.Origin != Origin {
		return Feed{}, fault.New(fault.Terminal, CodeOrigin,
			"notices: feed is not a "+Origin+" document")
	}
	if len(w.Notices) > MaxNotices {
		return Feed{}, fault.New(fault.Terminal, CodeTooLarge, "notices: feed carries too many notices")
	}

	f := Feed{
		Version: w.Version,
		Issued:  time.Unix(w.Issued, 0).UTC(),
		Expires: time.Unix(w.Expires, 0).UTC(),
		Notices: make([]Notice, 0, len(w.Notices)),
	}
	seen := make(map[string]bool, len(w.Notices))
	for _, wn := range w.Notices {
		id := Sanitize(wn.ID, MaxIDLen)
		if id == "" {
			return Feed{}, fault.New(fault.Terminal, CodeMalformed, "notices: a notice has no id")
		}
		// A duplicate id is refused rather than deduplicated: the client keys "already
		// shown" on the id, so two different notices under one id would mean showing the
		// first and permanently swallowing the second.
		if seen[id] {
			return Feed{}, fault.New(fault.Terminal, CodeMalformed, "notices: duplicate notice id "+id)
		}
		seen[id] = true

		sev := Severity(Sanitize(wn.Severity, MaxIDLen))
		if !sev.Valid() {
			return Feed{}, fault.New(fault.Terminal, CodeMalformed,
				"notices: notice "+id+" has an unknown severity")
		}
		summary := Sanitize(wn.Summary, MaxSummary)
		if summary == "" {
			return Feed{}, fault.New(fault.Terminal, CodeMalformed,
				"notices: notice "+id+" has no summary")
		}
		f.Notices = append(f.Notices, Notice{
			ID:           id,
			Severity:     sev,
			Summary:      summary,
			Detail:       Sanitize(wn.Detail, MaxDetail),
			URL:          Sanitize(wn.URL, MaxURLLen),
			AffectedFrom: Sanitize(wn.AffectedFrom, MaxVersionLen),
			FixedIn:      Sanitize(wn.FixedIn, MaxVersionLen),
		})
	}

	if len(w.Floors) > MaxFloors {
		return Feed{}, fault.New(fault.Terminal, CodeTooLarge, "notices: feed carries too many floors")
	}
	for _, wf := range w.Floors {
		runtime := Sanitize(wf.Runtime, MaxIDLen)
		version := Sanitize(wf.MinVersion, MaxVersionLen)
		// A floor with no runtime or no version gates nothing. It is dropped rather than
		// rejected: a malformed floor must not be able to take the whole feed down with
		// it, because the feed is also how a security advisory reaches the user.
		if runtime == "" || version == "" {
			continue
		}
		f.Floors = append(f.Floors, Floor{
			Runtime:    runtime,
			MinVersion: version,
			AdvisoryID: Sanitize(wf.AdvisoryID, MaxIDLen),
		})
	}
	return f, nil
}
