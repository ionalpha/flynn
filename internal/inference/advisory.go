package inference

import (
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Advisory names a known security issue in a runtime's model-file parser and the
// version that fixed it, so a refusal can say which issues a version is exposed to.
// The gate is the floor in [Runtime.MinSupported], which is at or above every advisory
// listed for its runtime.
type Advisory struct {
	Runtime   string  // matches Runtime.Name
	ID        string  // advisory identifier (a CVE, or a description where none was assigned)
	Summary   string  // one line on the issue
	FixedFrom Version // the first runtime version that resolves it
}

// advisories names the known model-parser issues, for reporting only.
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
	{
		Runtime:   "ollama",
		ID:        "ollama mllama parser RCE (no CVE assigned; fixed 0.7.0)",
		Summary:   "an out-of-bounds write while parsing a malicious model file allowed remote code execution; the unsafe C++ model handling was rewritten in 0.7.0",
		FixedFrom: Version{0, 7, 0},
	},
	{
		Runtime:   "vllm",
		ID:        "CVE-2025-62164",
		Summary:   "the completions API deserialized a user-supplied prompt-embeddings tensor with torch.load without validation, so a crafted request could corrupt memory and run code; the input is now parsed safely",
		FixedFrom: Version{0, 11, 1},
	},
	{
		Runtime:   "vllm",
		ID:        "CVE-2025-9141",
		Summary:   "the Qwen3-Coder tool-call parser passed an attacker-influenced argument into an unsafe evaluation, letting an authenticated request execute arbitrary code",
		FixedFrom: Version{0, 10, 1, 1},
	},
}

// Advisories returns a copy of the named advisory list.
func Advisories() []Advisory { return append([]Advisory(nil), advisories...) }

// Exposure returns the advisories in advs for runtime whose fix version v predates.
func Exposure(runtime string, v Version, advs []Advisory) []Advisory {
	var out []Advisory
	for _, a := range advs {
		if a.Runtime == runtime && v.Less(a.FixedFrom) {
			out = append(out, a)
		}
	}
	return out
}

// SafeToRun reports whether a runtime at version v may be used. It refuses a version
// below the runtime's floor and names the advisories it is exposed to, or says the
// version predates the last security fix when none is named. A runtime with no floor
// and no advisory is not judged here; an unfixed flaw is the sandbox's to contain.
func SafeToRun(runtime string, v Version) error {
	floor, hasFloor, raisedBy := FloorFor(runtime)
	belowFloor := hasFloor && v.Less(floor)
	exposed := Exposure(runtime, v, advisories)
	if !belowFloor && len(exposed) == 0 {
		return nil
	}

	ids := make([]string, 0, len(exposed)+1)
	for _, a := range exposed {
		ids = append(ids, a.ID)
	}
	// A floor raised after this build names an advisory the build has never heard of,
	// so its identifier gives the user something to look up.
	if belowFloor && raisedBy != "" {
		ids = append(ids, raisedBy)
	}
	var why string
	switch {
	case len(ids) > 0:
		why = "exposed to " + strings.Join(ids, ", ")
	default:
		why = fmt.Sprintf("older than the minimum supported %s version %s, which carries model-parser fixes", runtime, floor)
	}
	return fault.New(fault.Forbidden, "runtime_vulnerable",
		fmt.Sprintf("inference: %s %s is unsafe: %s; update the runtime", runtime, v, why))
}
