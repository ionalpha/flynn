package skillload

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/skill/skillmd"
)

func shell(cmd string, d time.Duration) Call {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return Call{Tool: "shell", Input: in, Elapsed: d}
}

func file(tool, p string) Call {
	in, _ := json.Marshal(map[string]string{"path": p})
	return Call{Tool: tool, Input: in}
}

func slugs(ls []Load) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Slug)
	}
	return out
}

func TestDecideFires(t *testing.T) {
	gomod := Candidate{Slug: "deps", Triggers: []skillmd.Trigger{{Kind: "file", Match: "**/go.mod"}}}
	cases := []struct {
		name  string
		cand  Candidate
		calls []Call
		want  bool
	}{
		{"file glob matches nested path", gomod, []Call{file("fs_write", "svc/api/go.mod")}, true},
		{"file glob matches bare name", gomod, []Call{file("fs_write", "go.mod")}, true},
		{"file glob matches windows separators", gomod, []Call{file("fs_write", `svc\api\go.mod`)}, true},
		{"file glob misses another file", gomod, []Call{file("fs_write", "svc/go.sum")}, false},
		{"file glob without path argument", gomod, []Call{shell("cat go.mod", 0)}, false},
		{"tool trigger", Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}}, []Call{shell("ls", 0)}, true},
		{"tool trigger other tool", Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "git"}}}, []Call{shell("ls", 0)}, false},
		{"slow at threshold", Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "slow", Seconds: 30}}}, []Call{shell("make", 30*time.Second)}, true},
		{"slow under threshold", Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "slow", Seconds: 30}}}, []Call{shell("make", 29*time.Second)}, false},
		{"slow narrowed to another tool", Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "slow", Seconds: 1, Match: "git"}}}, []Call{shell("make", time.Minute)}, false},
		{"no triggers never fires", Candidate{Slug: "s"}, []Call{shell("ls", time.Hour)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(Policy{}, State{}, []Candidate{tc.cand}, Observation{Calls: tc.calls})
			if got := len(d.Loads) == 1; got != tc.want {
				t.Fatalf("loaded = %v, want %v (%+v)", got, tc.want, d)
			}
		})
	}
}

func TestRepeatCountsAcrossObservations(t *testing.T) {
	c := Candidate{Slug: "feedback-loop", Triggers: []skillmd.Trigger{{Kind: "repeat", Count: 3}}}
	st := State{}
	for i := 1; i <= 3; i++ {
		d := Decide(Policy{}, st, []Candidate{c}, Observation{Calls: []Call{shell("go test ./...", 0)}})
		st = d.State
		if want := i == 3; (len(d.Loads) == 1) != want {
			t.Fatalf("run %d: loads = %+v, want load = %v", i, d.Loads, want)
		}
	}
	// A different command does not share the counter.
	d := Decide(Policy{}, State{}, []Candidate{c}, Observation{Calls: []Call{shell("a", 0), shell("b", 0), shell("c", 0)}})
	if len(d.Loads) != 0 {
		t.Fatalf("three different commands loaded %+v", d.Loads)
	}
}

func TestRepeatStateHoldsNoCommandText(t *testing.T) {
	d := Decide(Policy{}, State{}, nil, Observation{Calls: []Call{shell("curl -H 'Authorization: Bearer hunter2' x", 0)}})
	b, _ := json.Marshal(d.State)
	if string(b) == "" || strings.Contains(string(b), "hunter2") {
		t.Fatalf("state leaks the command: %s", b)
	}
}

func TestLoadsOncePerRun(t *testing.T) {
	c := Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}}
	obs := Observation{Calls: []Call{shell("ls", 0)}}
	d1 := Decide(Policy{}, State{}, []Candidate{c}, obs)
	d2 := Decide(Policy{}, d1.State, []Candidate{c}, obs)
	if len(d1.Loads) != 1 || len(d2.Loads) != 0 {
		t.Fatalf("loads = %d then %d, want 1 then 0", len(d1.Loads), len(d2.Loads))
	}
}

func TestModelReadSuppressesTrigger(t *testing.T) {
	c := Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}}
	d := Decide(Policy{}, State{}, []Candidate{c}, Observation{Calls: []Call{shell("ls", 0)}, Read: []string{"s"}})
	if len(d.Loads) != 0 || d.State.Triggered != 0 {
		t.Fatalf("a skill the model read was loaded again: %+v", d)
	}
}

func TestCapAndOrder(t *testing.T) {
	var cands []Candidate
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		cands = append(cands, Candidate{Slug: s, Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}})
	}
	obs := Observation{Calls: []Call{shell("ls", 0)}}
	d := Decide(Policy{}, State{}, cands, obs)
	if got, want := slugs(d.Loads), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("loads = %v, want %v", got, want)
	}
	if got, want := slugs(d.Suppressed), []string{"d", "e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("suppressed = %v, want %v", got, want)
	}
	// The next turn reports nothing new: suppression is recorded once.
	d2 := Decide(Policy{}, d.State, cands, obs)
	if len(d2.Loads) != 0 || len(d2.Suppressed) != 0 {
		t.Fatalf("second turn = %+v, want nothing", d2)
	}
}

func TestStartLoadsDoNotCountAgainstCap(t *testing.T) {
	st := State{}.MarkLoaded("named")
	var cands []Candidate
	for _, s := range []string{"a", "b", "c"} {
		cands = append(cands, Candidate{Slug: s, Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}})
	}
	d := Decide(Policy{}, st, cands, Observation{Calls: []Call{shell("ls", 0)}})
	if len(d.Loads) != 3 {
		t.Fatalf("loads = %v, want all three", slugs(d.Loads))
	}
}

func TestNegativeMaxDisablesTriggers(t *testing.T) {
	c := Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}}
	d := Decide(Policy{MaxTriggered: -1}, State{}, []Candidate{c}, Observation{Calls: []Call{shell("ls", 0)}})
	if len(d.Loads) != 0 {
		t.Fatalf("loads = %+v with triggers disabled", d.Loads)
	}
}

func TestDecideDoesNotMutateItsInput(t *testing.T) {
	st := State{Loaded: []string{"x"}, Repeats: map[string]int{"k": 1}}
	c := Candidate{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}}
	_ = Decide(Policy{}, st, []Candidate{c}, Observation{Calls: []Call{shell("ls", 0)}})
	if !reflect.DeepEqual(st, State{Loaded: []string{"x"}, Repeats: map[string]int{"k": 1}}) {
		t.Fatalf("input state changed: %+v", st)
	}
}

func TestStateSurvivesJSON(t *testing.T) {
	d := Decide(Policy{}, State{}, []Candidate{{Slug: "s", Triggers: []skillmd.Trigger{{Kind: "tool", Match: "shell"}}}}, Observation{Calls: []Call{shell("ls", 0)}})
	b, err := json.Marshal(d.State)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, d.State) {
		t.Fatalf("round trip = %+v, %v; want %+v", back, err, d.State)
	}
}

func TestRepeatCountersAreBounded(t *testing.T) {
	var calls []Call
	for i := 0; i < maxTracked+50; i++ {
		calls = append(calls, shell("cmd"+string(rune('a'+i%26))+string(rune('a'+i/26)), 0))
	}
	d := Decide(Policy{}, State{}, nil, Observation{Calls: calls})
	if len(d.State.Repeats) > maxTracked {
		t.Fatalf("%d counters, want at most %d", len(d.State.Repeats), maxTracked)
	}
}
