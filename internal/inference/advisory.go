package inference

import (
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// Advisory names a known security issue in a runtime's model-file parser and the
// version that fixed it. Advisories annotate the gate so a refusal can say which known
// issues a version is exposed to. They are not the gate themselves: the gate is the
// per-runtime minimum-version floor (Runtime.MinSupported), which catches every issue
// fixed before it, including the many runtime-parser flaws that ship with no CVE at
// all. A runtime's floor is therefore at or above every advisory listed for it.
type Advisory struct {
	Runtime   string  // matches Runtime.Name
	ID        string  // advisory identifier (a CVE, or a description where none was assigned)
	Summary   string  // one line on the issue
	FixedFrom Version // the first runtime version that resolves it
}

// advisories names the known model-parser issues, for reporting. The model-file parser
// in these runtimes is a recurring code-execution surface; many of its flaws are fixed
// without a CVE (for example the May 2026 batch of six llama.cpp GGUF-parser bugs, and
// the Ollama fix below), which is exactly why the version floor, not this list, is the
// gate.
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

// Exposure returns the advisories in advs that a runtime at version v is exposed to:
// those for that runtime whose fix this version predates.
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
// below the runtime's minimum-supported floor, which is the part that catches a whole
// class: everything fixed before the floor, named here or not. The refusal names the
// specific advisories the version is exposed to when there are any, so the reason is
// concrete, and otherwise reports that the version is simply older than the last
// security fix. A runtime with no floor and no advisory is not judged here.
//
// What this does not do: it cannot speak to an issue with no fix yet (an unpatched or
// unknown parser flaw, of which these runtimes have a steady supply). That risk is
// carried by running the runtime inside the sandbox, which contains a successful
// exploit whether or not the bug is known. The gate lowers the chance of one firing;
// the sandbox contains it when it does.
func SafeToRun(runtime string, v Version) error {
	floor, hasFloor := MinSupportedFor(runtime)
	belowFloor := hasFloor && v.Less(floor)
	exposed := Exposure(runtime, v, advisories)
	if !belowFloor && len(exposed) == 0 {
		return nil
	}

	ids := make([]string, len(exposed))
	for i, a := range exposed {
		ids[i] = a.ID
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
