package reliability

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// floorBits is the agentic-reliability floor in bits per weight. Below four-bit
// quantization (the Q4_K_M family), tool-call reliability and schema adherence degrade
// noticeably, and more so over a long loop. The bit width is a coarse a-priori warning
// the catalog shows before anything runs; the probe battery measures.
const floorBits = 4

// smallModelB is the parameter count in billions below which a model is small. A small
// model has the least headroom to absorb quantization, so even at the floor it gets a
// caution.
const smallModelB = 7

// quantBitsRE takes the leading bit width from a quant label: the digits after an
// optional I or Q prefix (Q4_K_M, IQ3_XS, Q8_0), or after f or fp (fp16, f16).
var quantBitsRE = regexp.MustCompile(`(?i)^(?:i?q|fp?)?(\d+)`)

// QuantFloor judges a model and quant against the agentic-reliability floor. paramsB is
// the parameter count in billions, 0 when unknown. The result is advisory: a quant at
// or above the floor still gets a caution for a small model, and an unrecognized label
// is reported unknown, so the catalog never implies a reliability it has not checked.
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

// quantBits extracts the bit width from a quant label; ok is false when there is none.
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
