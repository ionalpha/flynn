package learn

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/state"
)

// newStores builds real in-memory skill and memory facades over one resource
// store, so the curator is tested against the actual persistence path rather than
// a mock store.
func newStores(t *testing.T) (state.SkillStore, state.MemoryStore) {
	t.Helper()
	reg := resource.NewRegistry()
	for _, reg2 := range []func(*resource.Registry) error{
		resource.RegisterCoreKinds, skill.RegisterKind, memory.RegisterKind,
	} {
		if err := reg2(reg); err != nil {
			t.Fatal(err)
		}
	}
	rs := resource.NewMemory(reg)
	return skill.NewStore(rs), memory.NewStore(rs)
}

type fakeDistiller struct {
	lessons []Lesson
	err     error
	called  int
}

func (f *fakeDistiller) Distill(context.Context, Outcome) ([]Lesson, error) {
	f.called++
	return f.lessons, f.err
}

func convergedOutcome() Outcome {
	return Outcome{
		Objective: "do the thing",
		Result:    "did it",
		Converged: true,
		Scope:     state.Scope{Instance: "inst"},
		Source:    "run-1",
	}
}

func TestCuratorGatesOnConvergence(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{{Kind: LessonMemory, Body: "a fact"}}}
	c := NewCurator(d, skills, memories)

	o := convergedOutcome()
	o.Converged = false
	captured, err := c.Curate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Skills) != 0 || len(captured.Memories) != 0 {
		t.Fatalf("a non-converged run captured something: %+v", captured)
	}
	if d.called != 0 {
		t.Fatal("the distiller ran for a non-converged run; capture must be gated before distillation")
	}
}

func TestCuratorPersistsWithProvenance(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonSkill, Title: "Reset the Widget", Body: "Hold for 10s.", Tags: []string{"hardware"}},
		{Kind: LessonMemory, Body: "The widget firmware is v3."},
	}}
	c := NewCurator(d, skills, memories)
	ctx := context.Background()

	captured, err := c.Curate(ctx, convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Skills) != 1 || len(captured.Memories) != 1 {
		t.Fatalf("captured = %d skills, %d memories; want 1 each", len(captured.Skills), len(captured.Memories))
	}

	// The skill is retrievable by its slug and carries the learned-provenance tag.
	sk, err := skills.Get(ctx, "reset-the-widget")
	if err != nil {
		t.Fatalf("skill not stored under expected slug: %v", err)
	}
	if sk.Name != "Reset the Widget" || sk.Body != "Hold for 10s." {
		t.Fatalf("skill content = %+v", sk)
	}
	if !hasTag(sk.Tags, provenanceTag) || !hasTag(sk.Tags, "hardware") {
		t.Fatalf("skill tags = %v, want both 'hardware' and %q", sk.Tags, provenanceTag)
	}

	// The memory item is recallable and stamped with the run's source.
	items, err := memories.Recall(ctx, state.RecallQuery{Scope: state.Scope{Instance: "inst"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "The widget firmware is v3." {
		t.Fatalf("recalled = %+v", items)
	}
	if items[0].Source != "run-1" || items[0].Kind != memoryKind {
		t.Fatalf("memory provenance = source %q kind %q", items[0].Source, items[0].Kind)
	}
}

func TestCuratorSkipsEmptyBody(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonMemory, Body: "   "}, // whitespace-only: skipped
		{Kind: LessonSkill, Title: "real", Body: "keep me"},
	}}
	captured, err := NewCurator(d, skills, memories).Curate(context.Background(), convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Memories) != 0 || len(captured.Skills) != 1 {
		t.Fatalf("captured = %+v; empty-body lesson should be skipped", captured)
	}
}

func TestCuratorUpsertsSkillBySlug(t *testing.T) {
	skills, memories := newStores(t)
	ctx := context.Background()
	d := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "Same Title", Body: "v1"}}}
	c := NewCurator(d, skills, memories)

	if _, err := c.Curate(ctx, convergedOutcome()); err != nil {
		t.Fatal(err)
	}
	d.lessons = []Lesson{{Kind: LessonSkill, Title: "Same Title", Body: "v2"}}
	if _, err := c.Curate(ctx, convergedOutcome()); err != nil {
		t.Fatal(err)
	}

	all, err := skills.List(ctx, state.Scope{Instance: "inst"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("same-title lessons produced %d skills, want 1 (upsert by slug)", len(all))
	}
	if all[0].Body != "v2" {
		t.Fatalf("skill body = %q, want the updated v2", all[0].Body)
	}
}

func TestModelDistillerParsesReply(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  []Lesson
	}{
		{
			"clean array",
			`[{"kind":"skill","title":"T","body":"B","tags":["x"]},{"kind":"memory","body":"M"}]`,
			[]Lesson{{Kind: LessonSkill, Title: "T", Body: "B", Tags: []string{"x"}}, {Kind: LessonMemory, Body: "M"}},
		},
		{
			"wrapped in prose and a code fence",
			"Here are the lessons:\n```json\n[{\"kind\":\"memory\",\"body\":\"M\"}]\n```\nDone.",
			[]Lesson{{Kind: LessonMemory, Body: "M"}},
		},
		{"unknown kind defaults to memory", `[{"kind":"weird","body":"M"}]`, []Lesson{{Kind: LessonMemory, Body: "M"}}},
		{
			"skill with check",
			`[{"kind":"skill","title":"T","body":"B","check":"go test ./..."}]`,
			[]Lesson{{Kind: LessonSkill, Title: "T", Body: "B", Check: "go test ./..."}},
		},
		{"empty array", `[]`, nil},
		{"no array at all", `nothing structured here`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewModelDistiller(llmtest.NewScripted(llmtest.SayText(tc.reply)))
			got, err := d.Distill(context.Background(), convergedOutcome())
			if err != nil {
				t.Fatal(err)
			}
			if !equalLessons(got, tc.want) {
				t.Fatalf("lessons = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestModelDistillerRejectsMalformedArray(t *testing.T) {
	d := NewModelDistiller(llmtest.NewScripted(llmtest.SayText(`[{"kind": bad json}]`)))
	if _, err := d.Distill(context.Background(), convergedOutcome()); err == nil {
		t.Fatal("a malformed JSON array must be an error, not silently dropped")
	}
}

// TestCuratorWithModelDistillerEndToEnd drives the real path: a scripted model
// produces a lessons array, the model distiller parses it, and the curator persists
// it to the real stores, recallable afterward.
func TestCuratorWithModelDistillerEndToEnd(t *testing.T) {
	skills, memories := newStores(t)
	model := llmtest.NewScripted(llmtest.SayText(
		`[{"kind":"skill","title":"Deploy Safely","body":"Run tests first."},{"kind":"memory","body":"CI takes 3 min."}]`,
	))
	c := NewCurator(NewModelDistiller(model), skills, memories)

	captured, err := c.Curate(context.Background(), convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Skills) != 1 || len(captured.Memories) != 1 {
		t.Fatalf("captured = %+v", captured)
	}
	if _, err := skills.Get(context.Background(), "deploy-safely"); err != nil {
		t.Fatalf("learned skill not retrievable: %v", err)
	}
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func equalLessons(a, b []Lesson) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].Title != b[i].Title || a[i].Body != b[i].Body ||
			a[i].Check != b[i].Check || strings.Join(a[i].Tags, ",") != strings.Join(b[i].Tags, ",") {
			return false
		}
	}
	return true
}
