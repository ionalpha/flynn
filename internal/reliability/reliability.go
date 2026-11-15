// Package reliability measures whether a model is dependable enough to drive an agent
// loop, a separate question from whether a machine can run it. It runs a fixed,
// versioned battery of probes against any llm.Model and scores three capabilities that
// decide whether an agentic run finishes: well-formed tool calls, adherence to a tool's
// argument schema, and following direct instructions. The result maps to a
// harness.ModelProfile, which sets how hard the harness scaffolds the model. The
// battery is the same for every model, so a quantized local model and a hosted one are
// scored on identical terms.
package reliability

import (
	"context"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
)

// BatteryVersion pins the probe set. A score is comparable only to one taken at the
// same version, so it is recorded with the result and bumped when a probe changes.
const BatteryVersion = "1"

// dimension is one capability the battery measures. Each maps to a field of the
// profile, and the three together are the reliability fingerprint the harness reads.
type dimension int

const (
	dimToolCall    dimension = iota // emits a well-formed call to the right tool when one is needed
	dimStructured                   // the emitted arguments validate against the tool's schema
	dimInstruction                  // follows a direct, checkable instruction
)

// Report is the outcome of a battery run: per-dimension tallies and the battery
// version. The scores derive from the tallies, which stay available for a detailed
// report.
type Report struct {
	Version string
	Dims    map[dimension]Tally
}

// Tally counts the probes attempted and passed in one dimension.
type Tally struct {
	Passed    int
	Attempted int
}

// score is the pass fraction in [0,1]. An unattempted dimension scores 0, so a battery
// that could not exercise a capability does not vouch for it.
func (t Tally) score() float64 {
	if t.Attempted == 0 {
		return 0
	}
	return float64(t.Passed) / float64(t.Attempted)
}

// Profile maps the report to the harness's capability fingerprint. EffectiveContext is
// zero because this battery does not probe context length, and the harness treats the
// unknown conservatively.
func (r Report) Profile() harness.ModelProfile {
	return harness.ModelProfile{
		ToolCallReliability:  r.Dims[dimToolCall].score(),
		StructuredOutput:     r.Dims[dimStructured].score(),
		InstructionFollowing: r.Dims[dimInstruction].score(),
	}
}

// Score runs the battery against model and returns the tallied report. Each probe is
// one call with pinned decoding, so the measurement reproduces on a runtime that
// honors the seed. A probe whose call errors counts as failed, since a model that
// cannot answer is unreliable here. ctx bounds the whole run, so a cancelled
// measurement returns its error instead of a partial score.
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
