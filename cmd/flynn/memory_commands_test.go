package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/memory/guard"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// TestRenderSkillsAndMemory proves the browse renderers show the actual content, not
// just counts: a skill's name, verified flag, outcome record, and body preview; a
// memory item's kind and content.
func TestRenderSkillsAndMemory(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	if _, err := st.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy service", Body: "run the deploy script", Tags: []string{"verified"}, Uses: 3, Wins: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "Flynn is a Go agent runtime"}); err != nil {
		t.Fatal(err)
	}

	var sb bytes.Buffer
	renderSkills(ctx, &sb, st.Skills())
	for _, want := range []string{"Deploy service", "verified", "used 3", "won 2", "run the deploy script"} {
		if !strings.Contains(sb.String(), want) {
			t.Fatalf("skills render missing %q:\n%s", want, sb.String())
		}
	}

	var mb bytes.Buffer
	renderMemory(ctx, &mb, st.Memory())
	for _, want := range []string{"fact", "Flynn is a Go agent runtime"} {
		if !strings.Contains(mb.String(), want) {
			t.Fatalf("memory render missing %q:\n%s", want, mb.String())
		}
	}
}

// TestRenderLearningEmpty proves an empty store reports so rather than a blank.
func TestRenderLearningEmpty(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	var sb bytes.Buffer
	renderSkills(ctx, &sb, st.Skills())
	if !strings.Contains(sb.String(), "no skills") {
		t.Errorf("empty skills: %q", sb.String())
	}
	var mb bytes.Buffer
	renderMemory(ctx, &mb, st.Memory())
	if !strings.Contains(mb.String(), "no memory") {
		t.Errorf("empty memory: %q", mb.String())
	}
}

// TestShellMemoryAndSkillsBrowse proves /skills and /memory run as commands and list
// the stored content in the session.
func TestShellMemoryAndSkillsBrowse(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "ok"})
	ctx := context.Background()
	if _, err := host.s.store.Skills().Upsert(ctx, state.Skill{Slug: "deploy", Name: "Deploy service", Body: "run it"}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.s.store.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "Flynn is a Go agent runtime"}); err != nil {
		t.Fatal(err)
	}

	host.submit("/skills", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "Deploy service") {
		t.Fatalf("/skills did not list the skill:\n%s", ui.transcript())
	}
	host.submit("/memory", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "Flynn is a Go agent runtime") {
		t.Fatalf("/memory did not list the memory item:\n%s", ui.transcript())
	}
}

// TestRememberFactPins proves a pinned fact lands in durable memory with a
// user-scheme source, which the guard grades Trusted rather than the Semi a
// distilled item gets, and that the fact keeps its interior spacing.
func TestRememberFactPins(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()

	var out bytes.Buffer
	const fact = "the deploy target is Cloudflare Pages"
	if !rememberFact(ctx, &out, st.Memory(), "  "+fact+"  ") {
		t.Fatalf("rememberFact reported no write: %q", out.String())
	}
	if !strings.Contains(out.String(), "remembered: "+fact) {
		t.Errorf("no confirmation of what was kept: %q", out.String())
	}

	list, err := st.Memory().Recall(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 memory item, got %d", len(list))
	}
	got := list[0]
	if got.Content != fact {
		t.Errorf("content = %q, want %q (surrounding space trimmed, interior kept)", got.Content, fact)
	}
	if got.Kind != "fact" {
		t.Errorf("kind = %q, want %q", got.Kind, "fact")
	}
	if trust := guard.TrustOf(got.Source); trust != sandbox.TrustTrusted {
		t.Errorf("source %q grades %v, want %v: a hand-pinned fact must outrank a distilled one", got.Source, trust, sandbox.TrustTrusted)
	}

	// The browse surface distinguishes a pinned item from a distilled one.
	var mb bytes.Buffer
	renderMemory(ctx, &mb, st.Memory())
	if !strings.Contains(mb.String(), "[pinned]") {
		t.Errorf("/memory does not mark the pinned item:\n%s", mb.String())
	}
}

// TestRememberFactRejectsEmpty proves a bare /remember explains itself and writes
// nothing, rather than persisting an empty item.
func TestRememberFactRejectsEmpty(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()

	var out bytes.Buffer
	if rememberFact(ctx, &out, st.Memory(), "   ") {
		t.Error("rememberFact reported a write for an empty fact")
	}
	if !strings.Contains(out.String(), "usage: /remember") {
		t.Errorf("no usage line: %q", out.String())
	}
	list, err := st.Memory().Recall(ctx, state.RecallQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("wrote %d item(s) for an empty fact", len(list))
	}
}

// TestShellRememberCommand proves /remember runs as a session command and that the
// pinned fact is then visible to /memory in the same session.
func TestShellRememberCommand(t *testing.T) {
	host, ui := newHostForTest(t, constModel{text: "ok"})

	host.submit("/remember deploys go to Cloudflare", nil)
	waitIdle(t, host)
	if !strings.Contains(ui.transcript(), "remembered: deploys go to Cloudflare") {
		t.Fatalf("/remember did not confirm the write:\n%s", ui.transcript())
	}

	host.submit("/memory", nil)
	waitIdle(t, host)
	tr := ui.transcript()
	if !strings.Contains(tr, "[pinned]") || !strings.Contains(tr, "deploys go to Cloudflare") {
		t.Fatalf("/memory did not list the pinned fact:\n%s", tr)
	}
}
