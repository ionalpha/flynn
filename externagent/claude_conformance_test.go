package externagent

import "testing"

// TestClaudeProjectionFeedsConformanceLikeCodex proves the claude adapter drives the
// steering metrics the same way the codex adapter does: a bridged call the harness reports
// is not counted from the stream (the dispatch waist counts it directly), a native tool use
// is counted as drift, and assistant text and a tool result are not tool attempts. The
// conformance machinery is adapter-agnostic; this pins that the claude stream-json
// projection lands on the same typed events it reads.
func TestClaudeProjectionFeedsConformanceLikeCodex(t *testing.T) {
	c := NewClaude("", nil)
	const bridge = "flynn"

	parse := func(line string) []Event {
		evs, err := c.Parse([]byte(line))
		if err != nil {
			t.Fatalf("Parse(%s): %v", line, err)
		}
		return evs
	}

	var s Steering
	lines := []string{
		// One bridged tool call on the run's own server.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__flynn__write_file","input":{}}]}}`,
		// One native tool use (drift), which the waist never sees, so the stream is all there is.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}`,
		// The tool result for the bridged call and some assistant text are not tool attempts.
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	for _, line := range lines {
		for _, ev := range parse(line) {
			s.absorbSteering(ev, bridge)
		}
	}

	// The bridged call is not counted from the stream (the waist counts it directly, so a
	// harness that under-reports cannot deflate its own bridged count); the native tool use
	// is counted as drift. The same rule the codex projection is read under.
	if s.BridgeCalls != 0 {
		t.Errorf("the stream must not count bridged calls (the waist does), got %d", s.BridgeCalls)
	}
	if s.NativeCommands != 1 {
		t.Errorf("native commands = %d, want 1 (the shell tool)", s.NativeCommands)
	}

	// With the waist's own bridged count folded in, as the driver does, the native rate is
	// over every attempt, the same shape the codex projection produces.
	s.BridgeCalls = 1
	if s.Total() != 2 {
		t.Fatalf("total attempts = %d, want 2", s.Total())
	}
	if got := s.NativeRate(); got != 0.5 {
		t.Errorf("native rate = %v, want 0.5", got)
	}
}
