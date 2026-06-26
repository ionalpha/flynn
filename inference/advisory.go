package inference

import (
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Advisory is a known security advisory affecting a runtime's model-file parser,
// resolved at FixedFrom: a runtime at a version below FixedFrom is exposed to it.
type Advisory struct {
	Runtime   string  // matches Runtime.Name
	ID        string  // advisory identifier (a CVE)
	Summary   string  // one line on the issue
	FixedFrom Version // the first runtime version that resolves it
}

// advisories is the built-in advisory list: the known-exploitable model-file parser
// flaws and the runtime version each is fixed in. It is deliberately small and
// auditable. Keeping it current is ongoing maintenance, not a one-time list; entries
// carry the upstream identifier so each can be checked against its source.
var advisories = []Advisory{
	{
		Runtime:   "llama.cpp",
		ID:        "CVE-2025-49847",
		Summary:   "the vocabulary loader casts an oversized token length to a 32-bit int, bypassing a bounds check and overflowing a heap buffer; a malicious model can corrupt memory and run code",
		FixedFrom: Version{5662},
	},
	{
		Runtime:   "llama.cpp",
		ID:        "CVE-2026-27940",
		Summary:   "an integer overflow in the GGUF file parser (gguf_init_from_file_impl) undersizes a heap allocation and writes attacker-controlled data past the buffer; it completes the incomplete fix for CVE-2025-53630 in the same parser",
		FixedFrom: Version{8146},
	},
}

// Advisories returns a copy of the built-in advisory list.
func Advisories() []Advisory { return append([]Advisory(nil), advisories...) }

// Exposure returns the advisories in advs that a runtime at version v is exposed to:
// those for that runtime whose fix this version predates. An empty result means no
// advisory in the list applies to this runtime at this version.
func Exposure(runtime string, v Version, advs []Advisory) []Advisory {
	var out []Advisory
	for _, a := range advs {
		if a.Runtime == runtime && v.Less(a.FixedFrom) {
			out = append(out, a)
		}
	}
	return out
}

// SafeToRun reports whether a runtime at version v may be used, gating it against the
// built-in advisory list. It returns a Forbidden error naming the unpatched advisories
// when the version is exposed, so a caller refuses to run a model on a vulnerable
// parser rather than risk it. A runtime with no known advisory, or a version at or past
// every fix, is safe.
//
// A note on what this does not cover: a runtime whose version cannot be determined is
// not judged here (the caller decides whether an unknown version is acceptable), and a
// runtime with no entries in the list is reported safe only in the sense that nothing
// known applies, not that it has been audited.
func SafeToRun(runtime string, v Version) error {
	ex := Exposure(runtime, v, advisories)
	if len(ex) == 0 {
		return nil
	}
	ids := make([]string, len(ex))
	for i, a := range ex {
		ids[i] = a.ID
	}
	return fault.New(fault.Forbidden, "runtime_vulnerable",
		fmt.Sprintf("inference: %s %s is exposed to an unpatched model-parser advisory (%s); refusing to run a model on it, update the runtime",
			runtime, v, strings.Join(ids, ", ")))
}
