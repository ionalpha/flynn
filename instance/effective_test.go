package instance_test

import (
	"encoding/json"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/resource"
)

// base is a fixed reference time the derivation tests measure heartbeat ages
// against. It is arbitrary but constant, so every case is deterministic.
var base = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

// instanceRes builds an Instance resource with a given recorded state and
// heartbeat time, bypassing the store so the test controls UpdatedAt exactly. An
// empty state writes no status at all (the malformed/never-reconciled case).
func instanceRes(t *testing.T, state instance.State, heartbeat time.Time) resource.Resource {
	t.Helper()
	var status json.RawMessage
	if state != "" {
		b, err := json.Marshal(instance.Status{State: state})
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		status = b
	}
	return resource.Resource{
		Kind:     instance.Kind,
		Name:     "node-a",
		Status:   status,
		Envelope: resource.Envelope{UpdatedAt: heartbeat},
	}
}

func TestEffectiveState(t *testing.T) {
	const ttl = instance.DefaultStaleAfter
	fresh := base.Add(-ttl / 2) // within the window
	stale := base.Add(-2 * ttl) // well past it
	edge := base.Add(-ttl)      // exactly the window: not yet stale (strict)
	justOver := base.Add(-ttl - 1)

	cases := []struct {
		name      string
		state     instance.State
		heartbeat time.Time
		want      instance.State
	}{
		{"fresh idle stays idle", instance.StateIdle, fresh, instance.StateIdle},
		{"fresh working stays working", instance.StateWorking, fresh, instance.StateWorking},
		{"fresh blocked stays blocked", instance.StateBlocked, fresh, instance.StateBlocked},
		{"stale idle becomes unknown", instance.StateIdle, stale, instance.StateUnknown},
		{"stale working becomes unknown", instance.StateWorking, stale, instance.StateUnknown},
		{"stale blocked becomes unknown", instance.StateBlocked, stale, instance.StateUnknown},
		{"stale done stays done", instance.StateDone, stale, instance.StateDone},
		{"fresh done stays done", instance.StateDone, fresh, instance.StateDone},
		{"unknown stays unknown", instance.StateUnknown, fresh, instance.StateUnknown},
		{"missing status is unknown", "", fresh, instance.StateUnknown},
		{"edge heartbeat not yet stale", instance.StateWorking, edge, instance.StateWorking},
		{"one past edge is stale", instance.StateWorking, justOver, instance.StateUnknown},
		{"zero heartbeat is stale", instance.StateWorking, time.Time{}, instance.StateUnknown},
		{"zero heartbeat done stays done", instance.StateDone, time.Time{}, instance.StateDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := instance.EffectiveState(instanceRes(t, tc.state, tc.heartbeat), base, ttl)
			if got != tc.want {
				t.Fatalf("EffectiveState(%q, age) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

// TestEffectiveStateDisabledCheck verifies a non-positive staleAfter turns the
// staleness rule off entirely: even an ancient heartbeat keeps the live state, so
// callers can opt out of the heartbeat downgrade.
func TestEffectiveStateDisabledCheck(t *testing.T) {
	ancient := instanceRes(t, instance.StateWorking, base.Add(-1000*time.Hour))
	for _, ttl := range []time.Duration{0, -time.Second} {
		if got := instance.EffectiveState(ancient, base, ttl); got != instance.StateWorking {
			t.Fatalf("staleAfter=%v: EffectiveState = %q, want Working (check disabled)", ttl, got)
		}
	}
}

func TestIsStale(t *testing.T) {
	const ttl = time.Minute
	cases := []struct {
		name      string
		heartbeat time.Time
		ttl       time.Duration
		want      bool
	}{
		{"recent is fresh", base.Add(-30 * time.Second), ttl, false},
		{"old is stale", base.Add(-2 * time.Minute), ttl, true},
		{"exactly ttl is fresh", base.Add(-ttl), ttl, false},
		{"one ns over is stale", base.Add(-ttl - 1), ttl, true},
		{"zero heartbeat is stale", time.Time{}, ttl, true},
		{"disabled never stale", base.Add(-1000 * time.Hour), 0, false},
		{"negative ttl never stale", base.Add(-1000 * time.Hour), -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resource.Resource{Envelope: resource.Envelope{UpdatedAt: tc.heartbeat}}
			if got := instance.IsStale(r, base, tc.ttl); got != tc.want {
				t.Fatalf("IsStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProp_EffectiveStateInvariants checks the derivation's guarantees across
// arbitrary recorded states, heartbeat ages, and thresholds: the result is always
// one of the five known states; a live state downgrades to Unknown exactly when the
// heartbeat is stale; and terminal states (Done, Unknown) are never altered.
func TestProp_EffectiveStateInvariants(t *testing.T) {
	known := map[instance.State]bool{
		instance.StateIdle: true, instance.StateWorking: true, instance.StateBlocked: true,
		instance.StateDone: true, instance.StateUnknown: true,
	}
	live := map[instance.State]bool{
		instance.StateIdle: true, instance.StateWorking: true, instance.StateBlocked: true,
	}
	states := []instance.State{
		instance.StateIdle, instance.StateWorking, instance.StateBlocked,
		instance.StateDone, instance.StateUnknown,
	}

	rapid.Check(t, func(rt *rapid.T) {
		state := states[rapid.IntRange(0, len(states)-1).Draw(rt, "state")]
		ageSec := rapid.IntRange(-3600, 3600).Draw(rt, "ageSeconds")
		ttlSec := rapid.IntRange(1, 600).Draw(rt, "ttlSeconds")
		heartbeat := base.Add(-time.Duration(ageSec) * time.Second)
		ttl := time.Duration(ttlSec) * time.Second

		r := instanceRes(t, state, heartbeat)
		got := instance.EffectiveState(r, base, ttl)

		if !known[got] {
			rt.Fatalf("EffectiveState returned unknown vocabulary %q", got)
		}
		stale := instance.IsStale(r, base, ttl)
		switch {
		case live[state]:
			want := state
			if stale {
				want = instance.StateUnknown
			}
			if got != want {
				rt.Fatalf("live %q (stale=%v) -> %q, want %q", state, stale, got, want)
			}
		default: // Done or Unknown: terminal, never downgraded by heartbeat age
			if got != state {
				rt.Fatalf("terminal %q -> %q, want unchanged", state, got)
			}
		}
	})
}

// FuzzEffectiveState throws arbitrary status bytes and time offsets at the
// derivation: it must never panic and must always return a state in the known
// vocabulary, so malformed or adversarial stored status can never produce an
// out-of-band value or crash the read surface.
func FuzzEffectiveState(f *testing.F) {
	f.Add([]byte(`{"state":"Working"}`), int64(30), int64(90))
	f.Add([]byte(`{"state":"Done"}`), int64(99999), int64(90))
	f.Add([]byte(`{"state":"bogus"}`), int64(0), int64(90))
	f.Add([]byte(`not json`), int64(10), int64(5))
	f.Add([]byte(``), int64(-10), int64(1))

	known := map[instance.State]bool{
		instance.StateIdle: true, instance.StateWorking: true, instance.StateBlocked: true,
		instance.StateDone: true, instance.StateUnknown: true,
	}
	f.Fuzz(func(t *testing.T, status []byte, ageSec, ttlSec int64) {
		r := resource.Resource{
			Kind:     instance.Kind,
			Status:   json.RawMessage(status),
			Envelope: resource.Envelope{UpdatedAt: base.Add(-time.Duration(ageSec) * time.Second)},
		}
		got := instance.EffectiveState(r, base, time.Duration(ttlSec)*time.Second)
		if !known[got] {
			t.Fatalf("EffectiveState produced out-of-vocabulary state %q from status %q", got, status)
		}
	})
}
