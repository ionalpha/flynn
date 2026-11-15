// Package inference describes the local model runtimes Flynn can drive and gates
// them on their security posture.
//
// A runtime such as Ollama or llama.cpp parses model files, and that parser is a
// code-execution surface with a steady stream of memory-safety advisories. This
// package holds the runtime registry, a version model for the strings runtimes print,
// and the advisory gate that refuses a vulnerable version. It runs no process: the
// caller obtains a runtime's version output through the sandbox and passes it here.
//
// The gate is a minimum-version floor instead of a CVE list, because a floor catches
// every flaw fixed before it and most parser flaws ship with no CVE (six llama.cpp
// GGUF-parser bugs were disclosed in May 2026 with none assigned). The named
// advisories only make a refusal concrete. A flaw with no fix yet is contained by
// running the runtime inside the sandbox: the gate lowers the chance of an exploit
// firing, and the sandbox contains one that does.
package inference

import (
	"regexp"
	"strconv"
	"strings"
)

// Runtime identifies a local inference runtime and how to detect its version.
type Runtime struct {
	// Name is the stable identifier an advisory and a model spec refer to.
	Name string
	// Binaries are the executable names to look for, in preference order.
	Binaries []string
	// VersionArgs print the runtime's version, e.g. ["--version"].
	VersionArgs []string
	// versionRE picks the version token out of the build noise (commit hashes, build
	// dates, compiler versions) in the runtime's --version output. The first capture
	// group is the token to parse.
	versionRE *regexp.Regexp
	// MinSupported is the oldest version considered safe to run a model on: the most
	// recent release that fixed a known model-parser flaw. A lower version is refused.
	MinSupported Version
}

// The runtimes Flynn knows how to drive. Ollama is the default local runner; llama.cpp
// is the engine many runtimes build on and is supported directly.
var (
	// Ollama prints "ollama version is 0.3.14". Floor 0.7.0 removed the unsafe C++
	// model-file parser (the mllama out-of-bounds-write code execution).
	Ollama = Runtime{
		Name: "ollama", Binaries: []string{"ollama"}, VersionArgs: []string{"--version"},
		versionRE:    regexp.MustCompile(`(?i)version\D*(\d+\.\d+(?:\.\d+)?)`),
		MinSupported: Version{0, 7, 0},
	}
	// llama.cpp prints "version: 5662 (a1b2c3d)"; advisories are pinned to the build
	// number. Floor b8146 is the most recent GGUF-parser fix.
	LlamaCpp = Runtime{
		Name: "llama.cpp", Binaries: []string{"llama-server", "llama-cli"}, VersionArgs: []string{"--version"},
		versionRE:    regexp.MustCompile(`(?i)version\D*b?(\d+)`),
		MinSupported: Version{8146},
	}
	// vLLM prints a semver like "0.11.1". Its server deserializes request fields, so the
	// exposed surface is request handling as well as the model-file parser. Floor 0.11.1
	// fixes the prompt-embeddings torch.load deserialization and is at or above the
	// tool-parser and ZeroMQ deserialization fixes.
	VLLM = Runtime{
		Name: "vllm", Binaries: []string{"vllm"}, VersionArgs: []string{"--version"},
		versionRE:    regexp.MustCompile(`(\d+\.\d+\.\d+(?:\.\d+)?)`),
		MinSupported: Version{0, 11, 1},
	}
)

// MinSupportedFor returns the floor in force for a runtime by name: the higher of the
// version compiled into this binary and any floor passed to [Raise] since.
func MinSupportedFor(name string) (Version, bool) {
	v, ok, _ := FloorFor(name)
	return v, ok
}

// ParseVersion extracts this runtime's version from its raw --version output. It falls
// back to reading the whole string when the known format does not match.
func (r Runtime) ParseVersion(raw string) Version {
	if r.versionRE != nil {
		if m := r.versionRE.FindStringSubmatch(raw); m != nil {
			return ParseVersion(m[1])
		}
	}
	return ParseVersion(raw)
}

// Runtimes returns the known runtimes.
func Runtimes() []Runtime { return []Runtime{Ollama, LlamaCpp, VLLM} }

// Version is a runtime version as an ordered sequence of numeric components, so semver
// (0.3.14), build numbers (b3008) and tagged versions compare consistently within one
// runtime.
type Version []int

// maxComponentDigits caps a single numeric run so a hostile version string cannot
// overflow the parse.
const maxComponentDigits = 18

// ParseVersion reads every maximal run of digits in s as one component and ignores the
// text around them. It never fails: an input with no digits yields an empty Version.
func ParseVersion(s string) Version {
	var out Version
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j-i <= maxComponentDigits {
			if n, err := strconv.Atoi(s[i:j]); err == nil {
				out = append(out, n)
			}
		}
		i = j
	}
	return out
}

// Less reports whether v orders before o. A shorter prefix is the lesser when the shared
// components are equal, so 0.3 precedes 0.3.1.
func (v Version) Less(o Version) bool {
	for i := 0; i < len(v) && i < len(o); i++ {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return len(v) < len(o)
}

// String renders the version back to a dotted form for display.
func (v Version) String() string {
	if len(v) == 0 {
		return "unknown"
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}
