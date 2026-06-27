package reliability

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// floorBits is the agentic-reliability floor in bits per weight. Four-bit quantization (the
// Q4_K_M family) is the production floor: harsher than that degrades tool-call reliability and
// schema adherence noticeably, and the degradation compounds over a long loop. This is a coarse
// proxy, the bit width, not a measurement; the probe battery is the real signal, and this is the
// cheap a-priori warning the catalog can show before anything is run.
const floorBits = 4

// smallModelB is the parameter count (in billions) below which a model is considered small for
// this purpose. A small model has the least headroom to absorb quantization, so even at the
// four-bit floor it warrants a caution rather than a clean pass.
const smallModelB = 7

// quantBitsRE pulls the leading bit width out of a quant label: the digits after an optional I or
// Q prefix (Q4_K_M, IQ3_XS, Q8_0), or after an f/fp prefix for float formats (fp16, f16).
var quantBitsRE = regexp.MustCompile(`(?i)^(?:i?q|fp?)?(\d+)`)

// QuantFloor judges a model+quant against the agentic-reliability floor and returns whether it is
// below the floor together with a human-readable reason. paramsB is the parameter count in
// billions (0 when unknown). The result is advisory: a quant at or above the floor still gets a
// caution when the model is small, and an unrecognized quant label is reported as unknown rather
// than silently passed, so the catalog never implies a reliability it has not checked.
func QuantFloor(quant string, paramsB float64) (below bool, reason string) {
	bits, ok := quantBits(quant)
	switch {
	case !ok:
		return false, fmt.Sprintf("unrecognized quantization %q; agentic reliability not assessed from the label alone", quant)
	case bits < floorBits:
		return true, fmt.Sprintf("%s is below the Q4_K_M agentic-reliability floor (%d-bit); tool-call reliability degrades and the drift compounds over a long loop", quant, bits)
	case bits == floorBits && paramsB > 0 && paramsB < smallModelB:
		return false, fmt.Sprintf("%s is at the floor but the model is small (%.1fB); reliability is borderline and worsens with context length", quant, paramsB)
	default:
		return false, ""
	}
}

// quantBits extracts the bit width from a quant label, reporting ok=false when the label carries
// no recognizable width.
func quantBits(quant string) (int, bool) {
	m := quantBitsRE.FindStringSubmatch(strings.TrimSpace(quant))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
