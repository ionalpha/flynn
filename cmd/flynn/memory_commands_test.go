package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
