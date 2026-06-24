package spine

import "testing"

func TestUpcastRegistryApply(t *testing.T) {
	// A registry that bumps "moved" events from v1 to v2 by renaming the payload
	// key "old" to "new". Events already at v2, and events of other types, pass
	// through untouched.
	reg := UpcastRegistry{
		"moved": func(e Event) Event {
			if e.SchemaVersion >= 2 {
				return e
			}
			p := map[string]any{}
			for k, v := range e.Payload {
				p[k] = v
			}
			if v, ok := p["old"]; ok {
				p["new"] = v
				delete(p, "old")
			}
			e.Payload = p
			e.SchemaVersion = 2
			return e
		},
	}

	tests := []struct {
		name        string
		in          Event
		wantVersion int
		wantKey     string // payload key expected to hold the value "x"
	}{
		{
			name:        "old event is upcast",
			in:          Event{Type: "moved", SchemaVersion: 1, Payload: map[string]any{"old": "x"}},
			wantVersion: 2,
			wantKey:     "new",
		},
		{
			name:        "current event is unchanged",
			in:          Event{Type: "moved", SchemaVersion: 2, Payload: map[string]any{"new": "x"}},
			wantVersion: 2,
			wantKey:     "new",
		},
		{
			name:        "unregistered type passes through",
			in:          Event{Type: "other", SchemaVersion: 1, Payload: map[string]any{"old": "x"}},
			wantVersion: 1,
			wantKey:     "old",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.Apply(tt.in)
			if got.SchemaVersion != tt.wantVersion {
				t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, tt.wantVersion)
			}
			if got.Payload[tt.wantKey] != "x" {
				t.Errorf("payload[%q] = %v, want %q", tt.wantKey, got.Payload[tt.wantKey], "x")
			}
		})
	}

	// Apply must be deterministic: the same event yields an identical result every
	// time, which is what replay relies on.
	e := Event{Type: "moved", SchemaVersion: 1, Payload: map[string]any{"old": "x"}}
	first := reg.Apply(e)
	second := reg.Apply(e)
	if first.SchemaVersion != second.SchemaVersion || first.Payload["new"] != second.Payload["new"] {
		t.Errorf("Apply not deterministic: %+v vs %+v", first, second)
	}

	// A nil registry is usable and a no-op.
	var nilReg UpcastRegistry
	if got := nilReg.Apply(e); got.SchemaVersion != 1 {
		t.Errorf("nil registry mutated event: version = %d, want 1", got.SchemaVersion)
	}
}
