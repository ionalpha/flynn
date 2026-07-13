package externagent

import "testing"

// TestTierStringNamesEveryTier pins the provenance-tier names. They are a wire contract:
// the sealed record carries them and `flynn spine verify` reads them back to report how
// much of a run rests on the harness's own account rather than on what the run enforced.
// Renaming one would silently reclassify an old record, so every tier is pinned by name,
// and an out-of-range value reports "unknown" rather than an empty string that a reader
// could mistake for a missing field.
func TestTierStringNamesEveryTier(t *testing.T) {
	cases := []struct {
		tier Tier
		want string
	}{
		{TierEnforced, "enforced"},
		{TierAttested, "attested"},
		{TierUnobserved, "unobserved"},
		{Tier(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.tier.String(); got != tc.want {
			t.Errorf("Tier(%d).String() = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

// TestEventKindStringNamesEveryKind pins the event-kind names, which are the same wire
// contract: the record's readers match on them. A kind outside the set reports "unknown".
func TestEventKindStringNamesEveryKind(t *testing.T) {
	cases := []struct {
		kind EventKind
		want string
	}{
		{EventProgress, "progress"},
		{EventText, "text"},
		{EventUsage, "usage"},
		{EventError, "error"},
		{EventDone, "done"},
		{EventBridgeCall, "bridge_call"},
		{EventNativeCommand, "native_command"},
		{EventKind(42), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestReadinessReadyRequiresAllThree proves an episode starts only on a CLI that is
// installed, logged in, and not under a hard refusal. A refusal outranks a healthy probe:
// a build too old to constrain is never Ready however well it answers its version probe.
func TestReadinessReadyRequiresAllThree(t *testing.T) {
	cases := []struct {
		name string
		r    Readiness
		want bool
	}{
		{"installed and logged in", Readiness{Available: true, LoggedIn: true}, true},
		{"not installed", Readiness{LoggedIn: true}, false},
		{"logged out", Readiness{Available: true}, false},
		{"refused despite being healthy", Readiness{Available: true, LoggedIn: true, Refuse: true}, false},
		{"zero value", Readiness{}, false},
	}
	for _, tc := range cases {
		if got := tc.r.Ready(); got != tc.want {
			t.Errorf("%s: Ready() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWithSessionStampsOnlyARealID proves the conversation id an adapter read off a line
// is stamped on every event that line projected, and that a line carrying no id leaves the
// events alone. An id invented from an empty string would tell a later episode to resume a
// conversation that does not exist.
func TestWithSessionStampsOnlyARealID(t *testing.T) {
	evs := []Event{{Kind: EventProgress}, {Kind: EventText, Text: "hi"}}

	stamped := withSession(evs, "sess-1")
	for _, ev := range stamped {
		if ev.Session != "sess-1" {
			t.Errorf("event %v was not stamped with the conversation id: %+v", ev.Kind, ev)
		}
	}

	untouched := withSession([]Event{{Kind: EventProgress}}, "")
	if untouched[0].Session != "" {
		t.Errorf("a line announcing no conversation invented one: %q", untouched[0].Session)
	}
}
