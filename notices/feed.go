package notices

import (
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/ionalpha/flynn/fault"
)

// Origin names the feed and its format version inside the signed payload, so another
// document signed by the same key cannot be read as a feed, and a format change gets a
// new origin.
const Origin = "flynn/notices/v1"

// Limits on a feed. They bound memory and the screen, so a feed cannot bury an
// advisory under many entries or scroll it away.
const (
	MaxNotices    = 32
	MaxFloors     = 16
	MaxSummary    = 200
	MaxDetail     = 2000
	MaxIDLen      = 64
	MaxURLLen     = 200
	MaxVersionLen = 32
	// MaxFeedBytes caps the signed document. A real feed is a few kilobytes.
	MaxFeedBytes = 256 << 10
)

// Severity decides how a notice is shown. An unknown severity fails decoding.
type Severity string

const (
	// Security is a vulnerability in Flynn. It is shown on every run until the user
	// is on a fixed version.
	Security Severity = "security"
	// Deprecation is behaviour that will be removed or changed. It is shown once.
	Deprecation Severity = "deprecation"
	// Info is a release notice or announcement. It is shown once.
	Info Severity = "info"
)

// Valid reports whether s is a severity this client understands.
func (s Severity) Valid() bool {
	return s == Security || s == Deprecation || s == Info
}

// Failure codes. Each names the check that failed, so a refused advisory tells the
// user whether it was an attack or a bad file.
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

// Notice is one message for the user. Text fields are sanitized at decode.
type Notice struct {
	// ID is unique within a feed. A client remembers shown notices by ID, so reusing
	// one for new content suppresses it.
	ID       string
	Severity Severity
	// Summary is the one line the user sees.
	Summary string
	// Detail is the longer text shown by `flynn notices`. Optional.
	Detail string
	// URL is printed and never opened.
	URL string
	// AffectedFrom is the first version affected; empty means all earlier versions.
	AffectedFrom string
	// FixedIn is the first fixed version; empty means no fix has shipped.
	FixedIn string
}

// Floor is a minimum safe version for a local inference runtime, so a flaw disclosed
// after a release can raise the gate on installed binaries. It is the only part of a
// feed that acts, and it can only tighten: the client takes the higher of its own
// floor and this one (inference.Raise). An attacker holding the signing key can make
// Flynn refuse a runtime but never run a vulnerable one.
type Floor struct {
	// Runtime is "llama.cpp", "ollama" or "vllm"; another name is ignored.
	Runtime string
	// MinVersion uses the runtime's own scheme: a build number for llama.cpp, semver
	// for the others.
	MinVersion string
	// AdvisoryID tells the user what to read when this floor refuses a runtime.
	AdvisoryID string
}

// Feed is the signed document.
type Feed struct {
	// Version rises with every publication; [Accept] refuses one below the newest
	// trusted.
	Version uint64
	// Issued is when the feed was signed.
	Issued time.Time
	// Expires is set by a scheduled re-sign, so an expired feed means the client is
	// not seeing the current one.
	Expires time.Time
	// Notices is the complete current set, so a client that missed publications
	// misses no notices.
	Notices []Notice
	Floors  []Floor
}

// canonicalEnc is RFC 8949 core deterministic encoding. canonicalDec rejects
// duplicate map keys and indefinite lengths, so signed bytes have one meaning, as in
// chain/.
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

// wireFeed is the signed shape, kept apart from [Feed] so renaming a Go field cannot
// change the document, and times travel as Unix seconds.
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

// decodePayload treats verified bytes as hostile anyway, since the signing key can be
// misused. Every text field is sanitized here, the one place feed text enters, so no
// print site can forget.
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
		// Refused: shown state is keyed by id, so a second notice under the id would
		// never appear.
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
		// Dropped so a malformed floor cannot take the advisories in the same feed
		// down with it.
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
