package externagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// ProbeToolName is the bridged tool a session's reachability probe calls. It is named
// for what it does rather than for the run that serves it, so the external harness sees
// no fingerprint of who is governing it.
const ProbeToolName = "conformance_check"

// ProbeTool is an effect-free bridged tool whose only purpose is to prove that the
// external harness can reach the run's bridge and will use it when told to. It grants
// nothing: it touches no filesystem, runs no command, and reaches no network. Adding it
// to a run's grant therefore widens the harness's authority by exactly nothing, which is
// what makes it safe to admit unconditionally for the probe.
//
// Its value is that a call must cross the dispatch waist to arrive here. A harness that
// writes "I called conformance_check" into its output stream without dispatching does
// not move this tool, so the probe it settles cannot be passed by narration.
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

// Def describes the tool to the harness. The description states the contract the probe
// is testing, since the description is the only channel that reaches the harness's own
// tool-selection reasoning.
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

// probeInput is the tool's argument.
type probeInput struct {
	Nonce string `json:"nonce"`
}

// Invoke records that the harness reached the bridge with the right nonce. A call with a
// wrong or missing nonce is answered but not counted: it proves the channel is open, not
// that the harness followed the instruction it was given, and the probe is testing the
// latter.
//
// It is safe for concurrent use: the bridge serves the external subprocess, which may
// dispatch more than one call at a time.
func (p *ProbeTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	var in probeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	// Constant time, so the tool cannot be turned into an oracle that reveals the nonce
	// one byte at a time to a harness that is trying to guess it.
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
