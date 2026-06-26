// Package inference describes the local model runtimes Flynn can drive and gates
// them on their security posture.
//
// A local model is loaded and executed by a separate runtime (such as Ollama or
// llama.cpp). That runtime's model-file parser is a real code-execution surface, with
// a recurring stream of memory-safety advisories. Before Flynn runs a model on a
// runtime, it must know which runtime, at which version, and whether that version is
// exposed to a known-unpatched parser advisory. This package holds that knowledge as
// data: the runtime registry, a version model that compares the version strings the
// runtimes actually print, and the advisory gate that refuses a vulnerable runtime.
//
// It performs no process execution itself, so it stays pure and testable. A caller
// obtains a runtime's raw version output (through the sandbox boundary) and passes it
// here to parse and gate.
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
	// versionRE captures the version token out of the runtime's --version output,
	// which carries build noise (commit hashes, build dates, compiler versions) around
	// the one number that matters. The first capture group is the token to parse.
	versionRE *regexp.Regexp
}

// The runtimes Flynn knows how to drive. Ollama is the default local runner; llama.cpp
// is the lower-level engine many runtimes build on and is supported directly.
var (
	// Ollama prints "ollama version is 0.3.14".
	Ollama = Runtime{
		Name: "ollama", Binaries: []string{"ollama"}, VersionArgs: []string{"--version"},
		versionRE: regexp.MustCompile(`(?i)version[^\d]*(\d+\.\d+(?:\.\d+)?)`),
	}
	// llama.cpp prints "version: 5662 (a1b2c3d)"; the build number is what advisories
	// are pinned to.
	LlamaCpp = Runtime{
		Name: "llama.cpp", Binaries: []string{"llama-server", "llama-cli"}, VersionArgs: []string{"--version"},
		versionRE: regexp.MustCompile(`(?i)version[^\d]*b?(\d+)`),
	}
)

// ParseVersion extracts this runtime's version from its raw --version output, using
// the runtime's known format to pick the version token out of the surrounding build
// noise. It falls back to reading the whole string when the format does not match.
func (r Runtime) ParseVersion(raw string) Version {
	if r.versionRE != nil {
		if m := r.versionRE.FindStringSubmatch(raw); m != nil {
			return ParseVersion(m[1])
		}
	}
	return ParseVersion(raw)
}

// Runtimes returns the known runtimes.
func Runtimes() []Runtime { return []Runtime{Ollama, LlamaCpp} }

// Version is a runtime version as an ordered sequence of numeric components, so the
// many shapes runtimes print (semver like 0.3.14, a build number like b3008, a tag
// with a prefix or suffix) all compare consistently within one runtime. The components
// are whatever decimal numbers appear in the version string, in order.
type Version []int

// maxComponentDigits caps a single numeric run so a hostile or malformed version
// string cannot overflow the parse; real version components are far shorter.
const maxComponentDigits = 18

// ParseVersion extracts the numeric components from a version string. It reads every
// maximal run of digits as one component, ignoring any non-digit text around them, so
// it is robust to prefixes ("v", "b"), separators, and trailing build or tag noise. It
// never fails: an input with no digits yields an empty Version.
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

// Less reports whether v orders before o, comparing component by component and
// treating a shorter prefix as the lesser when the shared components are equal (so
// 0.3 precedes 0.3.1).
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
