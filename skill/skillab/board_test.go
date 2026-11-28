package skillab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/usage"
)

func TestSpreadOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []float64
		want Spread
	}{
		{"empty", nil, Spread{}},
		{"one", []float64{4}, Spread{N: 1, Median: 4, Lo: 4, Hi: 4}},
		{"odd", []float64{9, 1, 5}, Spread{N: 3, Median: 5, Lo: 1, Hi: 9}},
		{"even", []float64{4, 1, 3, 10}, Spread{N: 4, Median: 3.5, Lo: 1, Hi: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spreadOf(tc.in); got != tc.want {
				t.Fatalf("spreadOf(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSpreadFormat(t *testing.T) {
	if got := (Spread{}).format("", 0); got != "n/a" {
		t.Errorf("empty spread = %q", got)
	}
	if got := (Spread{N: 2, Median: 3, Lo: 3, Hi: 3}).format("", 0); got != "3" {
		t.Errorf("agreeing spread = %q, want the median alone", got)
	}
	if got := (Spread{N: 3, Median: 3, Lo: 1, Hi: 7}).format("", 0); got != "3 (1 to 7)" {
		t.Errorf("disagreeing spread = %q", got)
	}
}

func tok(in, out int64) usage.Tokens {
	r := func(n int64) usage.Count { return usage.Count{N: n, Basis: usage.Reported} }
	return usage.Tokens{Input: r(in), Output: r(out), CacheRead: r(0), CacheWrite: r(0)}
}

func sampleBoard() Board {
	return Board{
		Generated: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
		Prices:    "docs/scoreboard-prices.json",
		Arms: []ArmStatus{
			{Name: "flynn", Model: "anthropic:m"},
			{Name: "codex", Skipped: true, Reason: "codex is not installed"},
			{Name: "claude-code", Model: "m", Note: "a | b"},
		},
		Runs: []BoardRun{
			{Skill: "s", Fixture: "a", Arm: "flynn", Repeat: 1, Solved: true, Priced: true, USD: 1_000_000_000, Tokens: tok(100, 10), WallSeconds: 10, Turns: 4, FirstUseful: 2},
			{Skill: "s", Fixture: "a", Arm: "flynn", Repeat: 2, Solved: false, Priced: true, USD: 3_000_000_000, Tokens: tok(300, 30), WallSeconds: 30, Turns: 8},
			{Skill: "s", Fixture: "a", Arm: "claude-code", Repeat: 1, Solved: true, Priced: false, Tokens: tok(50, 5), WallSeconds: 5, Turns: 3, FirstUseful: 1},
			{Skill: "t", Fixture: "b", Arm: "flynn", Repeat: 1, Solved: true, Priced: true, USD: 2_000_000_000, Tokens: tok(200, 20), WallSeconds: 20, Turns: 6, FirstUseful: 3},
		},
	}
}

func TestByArmSkipsArmsThatDidNotRun(t *testing.T) {
	got := sampleBoard().ByArm()
	if len(got) != 2 || got[0].Arm != "flynn" || got[1].Arm != "claude-code" {
		t.Fatalf("ByArm arms = %+v, want flynn then claude-code and no skipped arm", got)
	}
	f := got[0]
	if f.Runs != 3 || f.Solved != 2 || f.USD != 6_000_000_000 || f.Unpriced != 0 {
		t.Fatalf("flynn tally = %+v", f)
	}
	if f.Tokens.Input.N != 600 || f.Tokens.Output.N != 60 {
		t.Errorf("flynn tokens = %+v, want the sum of the runs", f.Tokens)
	}
	if f.Wall != (Spread{N: 3, Median: 20, Lo: 10, Hi: 30}) {
		t.Errorf("flynn wall = %+v", f.Wall)
	}
	// The run that never edited a file is left out of turns to first edit.
	if f.FirstUseful != (Spread{N: 2, Median: 2.5, Lo: 2, Hi: 3}) {
		t.Errorf("flynn first useful = %+v", f.FirstUseful)
	}
	if v, ok := f.USDPerSolved(); !ok || v != 3 {
		t.Errorf("flynn USD per solve = %v, %t; want 3", v, ok)
	}
	if _, ok := got[1].USDPerSolved(); ok {
		t.Error("an arm with an unpriced run reported a USD per solve, which would read as complete")
	}
}

func TestByExerciseKeepsRunOrder(t *testing.T) {
	got := sampleBoard().ByExercise()
	var keys []string
	for _, tl := range got {
		keys = append(keys, tl.Exercise+":"+tl.Arm)
	}
	want := "s/a:flynn s/a:claude-code t/b:flynn"
	if strings.Join(keys, " ") != want {
		t.Fatalf("ByExercise = %v, want %s", keys, want)
	}
}

func TestMarkdownNamesSkippedArmsAndLowerBounds(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleBoard().Markdown(&buf); err != nil {
		t.Fatal(err)
	}
	md := buf.String()
	for _, want := range []string{
		"| codex | - | skipped | codex is not installed |",
		"| claude-code | m | yes | a / b |",
		"| flynn | 2/3 | 6.0000 | 3.0000 | 20 (10 to 30) | 6 (4 to 8) | 2 (2 to 3) | 600 / 60 / 0 / 0 |",
		"| claude-code | 1/1 | >= 0.0000 (1 unpriced) | n/a |",
		"| s/a | flynn | 1/2 | 2.0000 (1.0000 to 3.0000) |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

// An attempt that could not be evaluated is neither a solve nor a failure: its arm is
// shown as not measured, with the reason, and its spend is still counted.
func TestUnevaluatedAttemptsAreNotFailures(t *testing.T) {
	b := sampleBoard()
	b.Runs = append(b.Runs,
		BoardRun{Skill: "s", Fixture: "a", Arm: "flynn", Repeat: 3, Unevaluated: "no credits remaining", Priced: true, USD: 1_000_000_000, WallSeconds: 900},
		BoardRun{Skill: "u", Fixture: "c", Arm: "flynn", Repeat: 1, Unevaluated: "no credits remaining", Priced: true})
	f := b.ByArm()[0]
	if f.Runs != 3 || f.Solved != 2 || f.Unevaluated != 2 || f.USD != 7_000_000_000 || f.Wall.Hi != 30 {
		t.Fatalf("tally = %+v", f)
	}
	if _, ok := f.USDPerSolved(); ok {
		t.Error("an arm with a hole in it reported a USD per solve")
	}
	var buf bytes.Buffer
	if err := b.Markdown(&buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2 of those could not be evaluated.",
		"| flynn | anthropic:m | not measured | 2 attempt(s) could not be evaluated: no credits remaining |",
		"| flynn | not measured | 7.0000 | n/a |",
		"| s/a | flynn | 1/2 (1 could not be evaluated) |",
		"| u/c | flynn | could not evaluate |",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestRunBoardSkipsAttemptsAlreadyDone(t *testing.T) {
	entries := []BoardEntry{{Set: Set{Skill: "s"}, Exercise: Exercise{Fixture: "a"}}}
	var ran []int
	_, _, err := RunBoard(context.Background(), entries, []string{"x"}, 3, Limit{},
		func(_ BoardEntry, _ string, repeat int) bool { return repeat != 2 },
		func(_ context.Context, _ BoardEntry, _ string, repeat int) (BoardRun, Cost, error) {
			ran = append(ran, repeat)
			return BoardRun{}, Cost{}, nil
		})
	if err != nil || len(ran) != 1 || ran[0] != 2 {
		t.Fatalf("ran %v, err %v; want only repeat 2", ran, err)
	}
}

func TestRunBoardInterleavesArmsAndStopsAtTheLimit(t *testing.T) {
	entries := []BoardEntry{
		{Set: Set{Skill: "s"}, Exercise: Exercise{Fixture: "a"}},
		{Set: Set{Skill: "s"}, Exercise: Exercise{Line: 7}},
	}
	var order []string
	trial := func(_ context.Context, e BoardEntry, arm string, repeat int) (BoardRun, Cost, error) {
		skill, fixture := e.Key()
		order = append(order, fmt.Sprintf("%s/%s:%s:%d", skill, fixture, arm, repeat))
		return BoardRun{Arm: arm}, Cost{Turns: 1, Money: 1_000_000_000, Currency: "USD"}, nil
	}
	runs, stopped, err := RunBoard(context.Background(), entries, []string{"x", "y"}, 2, Limit{}, nil, trial)
	if err != nil || stopped != "" || len(runs) != 8 {
		t.Fatalf("unlimited board: %d runs, stopped %q, err %v", len(runs), stopped, err)
	}
	want := "s/a:x:1 s/a:y:1 s/a:x:2 s/a:y:2 s/line-7:x:1 s/line-7:y:1 s/line-7:x:2 s/line-7:y:2"
	if strings.Join(order, " ") != want {
		t.Errorf("order = %v, want %s", order, want)
	}

	runs, stopped, err = RunBoard(context.Background(), entries, []string{"x", "y"}, 2, Limit{Money: 3_000_000_000, Currency: "USD"}, nil, trial)
	if err != nil || len(runs) != 3 || !strings.Contains(stopped, "reached") {
		t.Fatalf("limited board: %d runs, stopped %q, err %v; want 3 and a reason", len(runs), stopped, err)
	}

	unpriced := func(context.Context, BoardEntry, string, int) (BoardRun, Cost, error) {
		return BoardRun{}, Cost{Unpriced: 1}, nil
	}
	runs, stopped, _ = RunBoard(context.Background(), entries, []string{"x"}, 1, Limit{Money: 1, Currency: "USD"}, nil, unpriced)
	if len(runs) != 1 || !strings.Contains(stopped, "could not be checked") {
		t.Fatalf("an unpriced attempt let the board go on under a limit: %d runs, %q", len(runs), stopped)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := RunBoard(ctx, entries, []string{"x"}, 1, Limit{}, nil, trial); err == nil {
		t.Error("a cancelled board ran on")
	}
}

// A board stops on a cancelled context or a trial's error and keeps nothing it did not
// run; repeats below one still run each attempt once.
func TestRunBoardEndsOnCancelAndTrialErrors(t *testing.T) {
	entries := []BoardEntry{{Set: Set{Skill: "s"}, Exercise: Exercise{Line: 3}}}
	if skill, fixture := entries[0].Key(); skill != "s" || fixture != "line-3" {
		t.Errorf("key = %s/%s, want an exercise without a fixture named by its line", skill, fixture)
	}
	calls := 0
	failing := func(context.Context, BoardEntry, string, int) (BoardRun, Cost, error) {
		calls++
		return BoardRun{}, Cost{}, errors.New("broken harness")
	}
	runs, _, err := RunBoard(context.Background(), entries, []string{"a"}, 0, Limit{}, nil, failing)
	if err == nil || len(runs) != 0 || calls != 1 {
		t.Errorf("trial error: runs %d, calls %d, err %v", len(runs), calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := RunBoard(ctx, entries, []string{"a"}, 1, Limit{}, nil, failing); err == nil || calls != 1 {
		t.Errorf("cancelled: calls %d, err %v", calls, err)
	}
	if _, err := ReadBoard(strings.NewReader("not json")); err == nil {
		t.Error("a file that is not a board was read")
	}
}

func TestBoardJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	b := sampleBoard()
	if err := b.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"usd": "1"`) {
		t.Errorf("USD is not written as a decimal string:\n%s", buf.String())
	}
	got, err := ReadBoard(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != len(b.Runs) || got.Runs[1].USD != b.Runs[1].USD || !got.Generated.Equal(b.Generated) {
		t.Fatalf("round trip = %+v", got)
	}
}
