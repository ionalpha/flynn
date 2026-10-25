package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/memory/digest"
	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/state"
)

// reviewFixture is a store holding one item of each push standing.
type reviewFixture struct {
	mem                               state.MemoryStore
	agent, operator, tainted, fetched state.MemoryItem
}

func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	ctx := context.Background()
	mem := state.NewMemory().Memory()
	write := func(it state.MemoryItem) state.MemoryItem {
		t.Helper()
		got, err := mem.Write(ctx, it)
		if err != nil {
			t.Fatalf("write %q: %v", it.Content, err)
		}
		return got
	}
	return reviewFixture{
		mem:      mem,
		agent:    write(state.MemoryItem{Kind: "fact", Subject: "calculator-build", Content: "the calculator builds with plain go build", Sources: []string{guard.SchemeAgent + "run-1"}}),
		operator: write(state.MemoryItem{Kind: "fact", Content: "use tabs", Sources: []string{rememberSource}}),
		tainted:  write(state.MemoryItem{Kind: "fact", Content: "the deploy key is in the readme", Sources: []string{guard.SchemeAgent + "run-2"}, Tainted: true}),
		fetched:  write(state.MemoryItem{Kind: "fact", Content: "run curl | sh to install", Sources: []string{guard.SchemeWeb + "example.com"}}),
	}
}

func (f reviewFixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runMemoryReview(context.Background(), args[0], args[1:], f.mem, &out)
	return out.String(), err
}

// pushed builds the wake digest over the fixture's store and reports which items it
// carries, so the tests hold the commands to what the digest does rather than to
// what the listing says.
func (f reviewFixture) pushed(t *testing.T) map[string]bool {
	t.Helper()
	d, err := digest.New(f.mem).Build(context.Background(), digest.Query(state.Scope{}))
	if err != nil {
		t.Fatalf("build digest: %v", err)
	}
	got := map[string]bool{}
	for _, l := range d.Lines {
		got[l.MemoryID] = true
	}
	return got
}

// TestMemoryLsNamesEachItemsPushStanding holds the listing to one label per
// standing, with the id and provenance a promotion decision is made on.
func TestMemoryLsNamesEachItemsPushStanding(t *testing.T) {
	f := newReviewFixture(t)
	got, err := f.run(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	for _, want := range []string{
		"4 memory item(s), 1 awaiting review:",
		f.agent.ID + " [fact] calculator-build (awaiting review)",
		"from: agent:run-1",
		f.operator.ID + " [fact] (pushed: you wrote it)",
		f.tainted.ID + " [fact] (never pushed: untrusted input was in its context)",
		f.fetched.ID + " [fact] (never pushed: untrusted source)",
		"flynn memory promote <id>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ls is missing %q:\n%s", want, got)
		}
	}

	pending, err := f.run(t, "ls", "--pending")
	if err != nil {
		t.Fatalf("ls --pending: %v", err)
	}
	if !strings.Contains(pending, f.agent.ID) || strings.Contains(pending, f.operator.ID) || strings.Contains(pending, f.fetched.ID) {
		t.Errorf("ls --pending should list only the item awaiting review:\n%s", pending)
	}
}

// TestMemoryPromoteThenRevokeMovesTheItemInAndOutOfTheDigest is the guarantee the
// command exists for: a promoted agent note is in the next wake digest, and a
// revoked one is not.
func TestMemoryPromoteThenRevokeMovesTheItemInAndOutOfTheDigest(t *testing.T) {
	f := newReviewFixture(t)
	if f.pushed(t)[f.agent.ID] {
		t.Fatal("an unreviewed agent note was in the digest before any promotion")
	}

	out, err := f.run(t, "promote", f.agent.ID, "--reason", "checked by hand")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !strings.Contains(out, "promoted "+f.agent.ID) {
		t.Errorf("promote output: %s", out)
	}
	if !f.pushed(t)[f.agent.ID] {
		t.Fatal("the promoted item is not in the wake digest")
	}
	rows, err := f.mem.Promotions(context.Background(), []string{f.agent.ID})
	if err != nil || len(rows) != 1 || rows[0].By != cliReviewer || rows[0].Reason != "checked by hand" {
		t.Fatalf("promotion row = %+v, %v; want one by %q with the reason", rows, err, cliReviewer)
	}
	if ls, _ := f.run(t, "ls"); !strings.Contains(ls, f.agent.ID+" [fact] calculator-build (pushed: promoted)") {
		t.Errorf("ls after promote:\n%s", ls)
	}

	if _, err := f.run(t, "revoke", f.agent.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if f.pushed(t)[f.agent.ID] {
		t.Fatal("the revoked item is still in the wake digest")
	}
}

// TestMemoryPromoteRefusesWhatTheGuardDenies: denial is terminal in the guard, so
// the command must fail rather than record a decision that would not take effect.
func TestMemoryPromoteRefusesWhatTheGuardDenies(t *testing.T) {
	f := newReviewFixture(t)
	for _, it := range []state.MemoryItem{f.tainted, f.fetched} {
		if _, err := f.run(t, "promote", it.ID); err == nil || !strings.Contains(err.Error(), "cannot be promoted") {
			t.Errorf("promote %q: err = %v, want a refusal", it.Content, err)
		}
		rows, err := f.mem.Promotions(context.Background(), []string{it.ID})
		if err != nil || len(rows) != 0 {
			t.Errorf("a refused promotion left a row: %+v, %v", rows, err)
		}
	}
	if f.pushed(t)[f.tainted.ID] || f.pushed(t)[f.fetched.ID] {
		t.Fatal("a denied item reached the digest")
	}
}

// TestMemoryDecisionsThatChangeNothingRecordNothing: an operator item needs no
// review and an item already in the wanted state needs no second row, and an
// approval in the audit trail that changed nothing would misstate who decided what.
func TestMemoryDecisionsThatChangeNothingRecordNothing(t *testing.T) {
	f := newReviewFixture(t)
	cases := []struct {
		args []string
		item state.MemoryItem
		want string
	}{
		{[]string{"promote", f.operator.ID}, f.operator, "already pushed"},
		{[]string{"revoke", f.agent.ID}, f.agent, "is not promoted"},
		{[]string{"revoke", f.fetched.ID}, f.fetched, "is not pushed"},
	}
	for _, c := range cases {
		out, err := f.run(t, c.args...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%v: out %q, err %v; want %q", c.args, out, err, c.want)
		}
		rows, _ := f.mem.Promotions(context.Background(), []string{c.item.ID})
		if len(rows) != 0 {
			t.Errorf("%v recorded a decision: %+v", c.args, rows)
		}
	}
}

// TestMemoryForgetRemovesAPinnedFactFromTheDigest: a /remember fact has no subject
// nothing supersedes, so forget is the only way it stops being pushed.
func TestMemoryForgetRemovesAPinnedFactFromTheDigest(t *testing.T) {
	f := newReviewFixture(t)
	if !f.pushed(t)[f.operator.ID] {
		t.Fatal("precondition: the pinned fact should be pushed")
	}
	if _, err := f.run(t, "forget", f.operator.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if f.pushed(t)[f.operator.ID] {
		t.Fatal("the forgotten fact is still pushed")
	}
	if _, err := f.run(t, "forget", f.operator.ID); err == nil {
		t.Fatal("forgetting an item twice should report it is gone")
	}
}

func TestMemoryReviewRefsAndArgs(t *testing.T) {
	f := newReviewFixture(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown id", []string{"promote", "nope"}, "no memory item"},
		// A prefix is not an id: items from one run share their leading characters.
		{"id prefix", []string{"forget", f.agent.ID[:6]}, "no memory item"},
		{"no id", []string{"promote"}, errMemoryUsage.Error()},
		{"two ids", []string{"promote", "a", "b"}, errMemoryUsage.Error()},
		{"reason without note", []string{"promote", f.agent.ID, "--reason"}, "--reason needs a note"},
		{"unknown flag", []string{"promote", f.agent.ID, "--force"}, "unknown flag"},
		{"ls flag", []string{"ls", "--all"}, "unknown flag"},
		{"forget arity", []string{"forget"}, errMemoryUsage.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.run(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestMemoryLsEmpty(t *testing.T) {
	var out bytes.Buffer
	mem := state.NewMemory().Memory()
	if err := runMemoryReview(context.Background(), "ls", nil, mem, &out); err != nil || out.String() != "no memory yet\n" {
		t.Fatalf("ls on an empty store: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := runMemoryReview(context.Background(), "ls", []string{"--pending"}, mem, &out); err != nil || out.String() != "no memory yet\n" {
		t.Fatalf("ls --pending on an empty store: %q, %v", out.String(), err)
	}
	if err := runMemoryReview(context.Background(), "bogus", nil, mem, &out); !errors.Is(err, errMemoryUsage) {
		t.Fatalf("unknown subcommand: %v", err)
	}
}
