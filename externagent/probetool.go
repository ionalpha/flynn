package externagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// ProbeToolName is the bridged tool a reachability probe calls. The name describes the
// check, so the external harness sees no fingerprint of who is governing it.
const ProbeToolName = "conformance_check"

// ProbeTool is an effect-free bridged tool that proves the external harness can reach
// the run's bridge and will use it when told to. It touches no filesystem, command or
// network, so admitting it to a grant widens the harness's authority by nothing.
//
// A call must cross the dispatch waist to arrive here, so a harness that only writes
// "I called conformance_check" into its output cannot pass the probe.
type ProbeTool struct {
	nonce string

	mu     sync.Mutex
	called bool
}

// NewProbeTool builds the probe tool for one episode. nonce must be unique per episode,
// so a harness cannot satisfy the probe by replaying an argument it saw earlier.
func NewProbeTool(nonce string) *ProbeTool {
	return &ProbeTool{nonce: nonce}
}

// Name is the tool name the harness calls and the action the waist admits.
func (*ProbeTool) Name() string { return ProbeToolName }

// Def states the contract under test in the description, because the description is
// the only channel that reaches the harness's own tool-selection reasoning.
func (*ProbeTool) Def() llm.Tool {
	return llm.Tool{
		Name: ProbeToolName,
		Description: "Confirms this session's tool channel is reachable. Call it once, first, " +
			"with the nonce you were given. It has no other effect.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"nonce": {"type": "string", "description": "the nonce provided in the instructions"}},
			"required": ["nonce"]
		}`),
	}
}

type probeInput struct {
	Nonce string `json:"nonce"`
}

// Invoke records that the harness reached the bridge with the right nonce. A call with
// a wrong or missing nonce is answered but not counted: it proves the channel is open,
// and the probe tests whether the harness followed its instruction. It is safe for
// concurrent use, because the subprocess may dispatch several calls at once.
func (p *ProbeTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	var in probeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	// Constant time, so the tool cannot become an oracle that leaks the nonce one byte
	// at a time.
	if subtle.ConstantTimeCompare([]byte(in.Nonce), []byte(p.nonce)) == 1 {
		p.mu.Lock()
		p.called = true
		p.mu.Unlock()
		return "ok", nil
	}
	return "nonce did not match; the channel is reachable but the instruction was not followed", nil
}

// Called reports whether the harness dispatched a call carrying the episode's nonce.
func (p *ProbeTool) Called() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called
}

var _ mission.Tool = (*ProbeTool)(nil)
