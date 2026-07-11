package externagent

import "testing"

// FuzzCodexParse drives the codex `exec --json` line decode from raw bytes. Each
// line is untrusted output from an external CLI, so the bar is that no input
// panics and Parse never returns an error: a line it cannot decode is kept as
// attested progress carrying the raw bytes, never dropped and never fatal. On any
// non-empty (post-trim) line Parse must emit at least one event so nothing the CLI
// wrote is silently lost.
func FuzzCodexParse(f *testing.F) {
	seeds := []string{
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"type":"error","message":"unauthorized"}`,
		`{"type":"turn.failed","error":{"message":"boom"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"hi"}}`,
		`{"type":"item.started","item":{"type":"assistant_message","text":""}}`,
		`{"type":"thread.started"}`,
		`{"type":123}`,      // wrong scalar type for the discriminant
		`{"usage":null}`,    // null nested object
		`   {"type":"x"}  `, // surrounding whitespace
		"a banner line",     // CLI noise, not JSON
		"",
		"\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	c := NewCodex("", nil)
	f.Fuzz(func(t *testing.T, line []byte) {
		evs, err := c.Parse(line)
		if err != nil {
			t.Fatalf("Parse returned a non-nil error for %q: %v", line, err)
		}
		if len(trimLine(line)) > 0 && len(evs) == 0 {
			t.Fatalf("Parse dropped a non-empty line %q (produced no events)", line)
		}
		for _, ev := range evs {
			if ev.Kind == EventProgress && len(ev.Raw) == 0 && len(trimLine(line)) > 0 {
				t.Fatalf("progress event for %q carries empty Raw", line)
			}
		}
	})
}

// FuzzClaudeParse drives the claude stream-json line decode from raw bytes, the same
// bar as the codex parser: no input panics, Parse never errors, and a non-empty
// post-trim line always emits at least one event so nothing the CLI wrote is dropped.
// An assistant message with several blocks, or a result line, may emit more than one
// event, so this does not bound the count from above.
func FuzzClaudeParse(f *testing.F) {
	seeds := []string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__flynn__x","input":{"a":1}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"unauthorized"}`,
		`{"type":"assistant","message":null}`,
		`{"type":123}`,
		`   {"type":"x"}  `,
		"a banner line",
		"",
		"\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	c := NewClaude("", nil)
	f.Fuzz(func(t *testing.T, line []byte) {
		evs, err := c.Parse(line)
		if err != nil {
			t.Fatalf("Parse returned a non-nil error for %q: %v", line, err)
		}
		if len(trimLine(line)) > 0 && len(evs) == 0 {
			t.Fatalf("Parse dropped a non-empty line %q (produced no events)", line)
		}
		for _, ev := range evs {
			if ev.Tier < TierEnforced || ev.Tier > TierUnobserved {
				t.Fatalf("event for %q has an out-of-range tier: %v", line, ev.Tier)
			}
		}
	})
}
