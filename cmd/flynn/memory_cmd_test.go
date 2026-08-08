package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory/consolidate"
	"github.com/ionalpha/flynn/state"
)

func TestDispatchMemoryUsage(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"list"}, {"consolidated"}} {
		if err := dispatchMemory(args, "", t.TempDir(), &bytes.Buffer{}); !errors.Is(err, errMemoryUsage) {
			t.Errorf("dispatchMemory(%q) = %v, want the usage error", args, err)
		}
	}
	// The subcommand that exists reaches the sweep, which then fails on its own
	// terms rather than on the routing.
	err := dispatchMemory([]string{"consolidate"}, "nosuchprovider:nosuchmodel", t.TempDir(), &bytes.Buffer{})
	if err == nil || errors.Is(err, errMemoryUsage) {
		t.Errorf("dispatchMemory(consolidate) = %v, want a failure that is not the usage error", err)
	}
}

func TestParseConsolidateArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{name: "no flags is no cap", args: nil},
		{name: "a cap", args: []string{"--max-calls", "7"}, want: 7},
		{name: "an explicit zero declines every subject", args: []string{"--max-calls", "0"}},
		{name: "a missing number", args: []string{"--max-calls"}, wantErr: true},
		{name: "not a number", args: []string{"--max-calls", "lots"}, wantErr: true},
		{name: "a negative cap", args: []string{"--max-calls", "-1"}, wantErr: true},
		{name: "an unknown flag", args: []string{"--everything"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConsolidateArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseConsolidateArgs(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("maxCalls = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestConsolidateSweepOverTheShippedPass runs the sweep the command runs, on the
// distiller the command builds, over a store seeded the way a run seeds one: a
// subject whose failures accumulated into a series.
//
// What the command wraps around this is a store open and a model resolve, neither
// of which a test can do without credentials. Everything else is here: the series
// becomes one lesson, the episodes it was drawn from are gone, and the report says
// so.
func TestConsolidateSweepOverTheShippedPass(t *testing.T) {
	ctx := context.Background()
	st := state.NewMemory().Memory()
	for _, content := range []string{"the deploy failed", "it failed again", "it failed once the migration moved"} {
		if _, err := st.Write(ctx, state.MemoryItem{
			Kind: consolidate.KindEpisode, Subject: "deploy-api", Content: content, Sources: []string{"agent:run-1"},
		}); err != nil {
			t.Fatalf("seed %q: %v", content, err)
		}
	}

	model := llmtest.NewScripted(llmtest.SayText("The deploy fails when the migration runs after it. Run the migration first."))
	var out bytes.Buffer
	if err := runConsolidation(ctx, st, consolidateDistiller(model, 0), &out); err != nil {
		t.Fatalf("runConsolidation: %v", err)
	}

	lessons := recallSubject(t, ctx, st, "deploy-api", consolidate.KindLesson)
	if len(lessons) != 1 || !strings.Contains(lessons[0].Content, "migration") {
		t.Fatalf("lessons = %+v, want the one the model drew", lessons)
	}
	if left := recallSubject(t, ctx, st, "deploy-api", consolidate.KindEpisode); len(left) != 0 {
		t.Fatalf("episodes left = %d, want 0: the lesson took them over", len(left))
	}

	for _, want := range []string{"1 distilled", "deploy-api", "3 episode(s) retired"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
}

// TestConsolidateCapDeclinesRatherThanFailing proves the spend cap leaves the
// store as it found it. An operator who capped a sweep gets fewer lessons, not a
// half-consolidated subject and a report of failures.
func TestConsolidateCapDeclinesRatherThanFailing(t *testing.T) {
	ctx := context.Background()
	st := state.NewMemory().Memory()
	for _, subject := range []string{"aaa", "bbb"} {
		for _, content := range []string{"one", "two", "three"} {
			if _, err := st.Write(ctx, state.MemoryItem{
				Kind: consolidate.KindEpisode, Subject: subject, Content: subject + "-" + content, Sources: []string{"agent:run-1"},
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	model := llmtest.NewScripted(llmtest.SayText("a lesson"))
	pass, err := consolidate.New(st, consolidateDistiller(model, 1))
	if err != nil {
		t.Fatalf("build the pass: %v", err)
	}
	rep, err := pass.Run(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Distilled() != 1 || len(rep.Failures) != 0 {
		t.Fatalf("distilled = %d, failures = %+v, want exactly one and no failures", rep.Distilled(), rep.Failures)
	}
	if left := recallSubject(t, ctx, st, "bbb", consolidate.KindEpisode); len(left) != 3 {
		t.Fatalf("episodes left on the uncapped-out subject = %d, want all 3 for the next sweep", len(left))
	}

	var out bytes.Buffer
	reportConsolidation(&out, rep)
	if !strings.Contains(out.String(), "1 declined") {
		t.Errorf("report does not say a subject was declined:\n%s", out.String())
	}
}

// TestReportConsolidationNamesAFailedSubject pins that the sweep carrying on past
// a failure only holds up if the failure is visible afterwards.
func TestReportConsolidationNamesAFailedSubject(t *testing.T) {
	var out bytes.Buffer
	reportConsolidation(&out, consolidate.Report{
		Results:  []consolidate.Result{{Subject: "quiet-subject", Outcome: consolidate.OutcomeTooFew, Episodes: 2}},
		Failures: []consolidate.Failure{{Subject: "deploy-api", Err: errors.New("the model timed out")}},
	})
	if !strings.Contains(out.String(), "1 not yet a series") {
		t.Errorf("report does not count the subject that is short of a series:\n%s", out.String())
	}
	for _, want := range []string{"deploy-api", "the model timed out"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
}

// TestConsolidateMemoryRefusesBeforeSpending pins the order the command does
// things in: a bad flag or an unresolvable model stops it before it opens a store
// or spends anything, so a mistyped invocation costs nothing.
func TestConsolidateMemoryRefusesBeforeSpending(t *testing.T) {
	tests := []struct {
		name string
		args []string
		spec string
	}{
		{name: "a bad flag", args: []string{"--max-calls", "lots"}, spec: "anthropic:claude"},
		{name: "an unknown flag", args: []string{"--sweep"}, spec: "anthropic:claude"},
		{name: "no model to distil through", spec: "nosuchprovider:nosuchmodel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := consolidateMemory(tt.args, tt.spec, t.TempDir(), &out); err == nil {
				t.Fatal("consolidateMemory = nil error, want a refusal")
			}
			if out.Len() != 0 {
				t.Fatalf("wrote %q, want nothing: it refused before it did any work", out.String())
			}
		})
	}
}

func TestRunConsolidationReportsItsOwnFailures(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer

	// consolidate refuses at construction rather than on the first subject, so a
	// sweep wired up wrong says so before it reads anything.
	if err := runConsolidation(ctx, state.NewMemory().Memory(), nil, &out); !errors.Is(err, consolidate.ErrNoDistiller) {
		t.Fatalf("runConsolidation with no distiller = %v, want ErrNoDistiller", err)
	}
	// A store that cannot be read fails the sweep, where a subject that cannot be
	// distilled would only fail itself.
	want := errors.New("the store is gone")
	broken := failingMemory{MemoryStore: state.NewMemory().Memory(), err: want}
	if err := runConsolidation(ctx, broken, consolidateDistiller(llmtest.NewScripted(), 0), &out); !errors.Is(err, want) {
		t.Fatalf("runConsolidation over an unreadable store = %v, want the read failure", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q, want nothing: neither sweep got as far as a report", out.String())
	}
}

// TestRunMemoryCommandRouting proves `flynn memory` reaches the sweep and that a
// subcommand nobody has written is a usage error rather than a crash.
func TestRunMemoryCommandRouting(t *testing.T) {
	if got := runCLI(t, "memory"); got.code != 2 || !strings.Contains(got.stderr, "flynn memory consolidate") {
		t.Errorf("flynn memory = %+v, want the usage line and exit 2", got)
	}
	if got := runCLI(t, "memory", "forget"); got.code != 2 {
		t.Errorf("flynn memory forget = %+v, want exit 2", got)
	}
	// With no credential configured the sweep cannot resolve a model, which is a
	// command failure and not a usage error.
	got := runCLI(t, "memory", "consolidate")
	if got.code != 1 || got.stdout != "" {
		t.Errorf("flynn memory consolidate = %+v, want exit 1 and no report", got)
	}
}
