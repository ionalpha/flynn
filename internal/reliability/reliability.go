// Package reliability measures whether a model is dependable enough to drive an agent loop,
// which is a different question from whether a machine can run it. It runs a fixed, version
// pinned battery of probes against any llm.Model and scores three capabilities that decide
// whether an agentic run finishes or stalls: emitting well-formed tool calls, adhering to a
// tool's argument schema, and following direct instructions. The result maps to a
// harness.ModelProfile, so a measurement made here drives how hard the harness scaffolds that
// model at the dispatch waist. The battery is the same regardless of which model is behind the
// port, so a quantized local model and a hosted one are scored on identical terms.
package reliability

import (
	"context"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
)

// BatteryVersion pins the probe set. A score is only comparable to another taken at the same
// version, so it is recorded alongside the result and bumped whenever a probe changes.
const BatteryVersion = "1"

// dimension is one capability the battery measures. Each maps to a field of the resulting
// profile, so the three together are exactly the reliability fingerprint the harness reads.
type dimension int

const (
	dimToolCall    dimension = iota // emits a well-formed call to the right tool when one is needed
	dimStructured                   // the emitted arguments validate against the tool's schema
	dimInstruction                  // follows a direct, checkable instruction
)

// Report is the outcome of running the battery: how many probes passed in each dimension, and
// the battery version the run used. The scores are derived from it, so the raw tallies stay
// available for a detailed report.
type Report struct {
	Version string
	Dims    map[dimension]Tally
}

// Tally counts the probes attempted and passed in one dimension.
type Tally struct {
	Passed    int
	Attempted int
}

// score is the pass fraction in [0,1]; an unattempted dimension scores 0, the conservative
// unknown, so a battery that could not exercise a capability does not vouch for it.
func (t Tally) score() float64 {
	if t.Attempted == 0 {
		return 0
	}
	return float64(t.Passed) / float64(t.Attempted)
}

// Profile maps the report to the capability fingerprint the harness consumes. EffectiveContext
// is left zero: this battery does not probe context length, so it makes no claim about it, and
// the harness treats the unknown conservatively. The other three dimensions map directly.
func (r Report) Profile() harness.ModelProfile {
	return harness.ModelProfile{
		ToolCallReliability:  r.Dims[dimToolCall].score(),
		StructuredOutput:     r.Dims[dimStructured].score(),
		InstructionFollowing: r.Dims[dimInstruction].score(),
	}
}

// Score runs the full battery against model and returns the tallied report. Each probe is one
// model call with pinned decoding, so the measurement is reproducible on a runtime that honors
// the seed. A probe whose call errors counts as a failure, not a skipped probe: a model that
// cannot answer reliably is, for this purpose, unreliable. The context bounds the whole run, so
// a cancelled measurement returns its error rather than a partial score.
func Score(ctx context.Context, model llm.Model) (Report, error) {
	rep := Report{Version: BatteryVersion, Dims: map[dimension]Tally{}}
	for _, p := range battery() {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		t := rep.Dims[p.dim]
		t.Attempted++
		resp, err := model.Generate(ctx, p.req)
		if err == nil && p.grade(resp) {
			t.Passed++
		}
		rep.Dims[p.dim] = t
	}
	return rep, nil
}
