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
