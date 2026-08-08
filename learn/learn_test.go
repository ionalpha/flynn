package learn

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/memory"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/skillmd"
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
	if !slices.Equal(items[0].Sources, []string{"run-1"}) || items[0].Kind != memoryKind {
		t.Fatalf("memory provenance = sources %v kind %q", items[0].Sources, items[0].Kind)
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

func TestCuratorDoesNotOverwriteAuthoredSkill(t *testing.T) {
	skills, memories := newStores(t)
	ctx := context.Background()
	scope := state.Scope{Instance: "inst"}

	// An authored skill: same slug the lesson below will produce, but no
	// learned-provenance tag, so it is curated content the loop must protect.
	authored, err := skills.Upsert(ctx, state.Skill{
		Slug:  "reset-the-widget",
		Name:  "Reset the Widget",
		Body:  "The authored procedure.",
		Check: "test -f widget",
		Tags:  []string{"hardware"},
		Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}

	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonSkill, Title: "Reset the Widget", Body: "A learned procedure.", Check: "false"},
	}}
	captured, err := NewCurator(d, skills, memories).Curate(ctx, convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}

	if len(captured.Skills) != 0 {
		t.Fatalf("captured %d skills; a collision with an authored skill must store nothing", len(captured.Skills))
	}
	if len(captured.Skipped) != 1 {
		t.Fatalf("Skipped = %d, want 1 so the caller can see the capture was refused", len(captured.Skipped))
	}

	// The authored skill is untouched: body, check and version all unchanged.
	got, err := skills.Get(ctx, "reset-the-widget")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "The authored procedure." || got.Check != "test -f widget" {
		t.Fatalf("authored skill was modified: %+v", got)
	}
	if got.Version != authored.Version {
		t.Fatalf("authored skill version moved from %d to %d; it should not have been written", authored.Version, got.Version)
	}
	if hasTag(got.Tags, provenanceTag) {
		t.Fatalf("authored skill gained the learned tag: %v", got.Tags)
	}
}

func TestCuratorReCapturesItsOwnLearnedSkill(t *testing.T) {
	skills, memories := newStores(t)
	ctx := context.Background()
	d := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "Learned Thing", Body: "v1"}}}
	c := NewCurator(d, skills, memories)

	if _, err := c.Curate(ctx, convergedOutcome()); err != nil {
		t.Fatal(err)
	}
	// A learned skill carries the provenance tag, so re-capturing it updates rather
	// than being refused as if it were authored.
	d.lessons = []Lesson{{Kind: LessonSkill, Title: "Learned Thing", Body: "v2"}}
	captured, err := c.Curate(ctx, convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Skipped) != 0 {
		t.Fatalf("re-capturing a learned skill was skipped: %+v", captured.Skipped)
	}
	if len(captured.Skills) != 1 {
		t.Fatalf("captured = %d skills, want 1", len(captured.Skills))
	}
	got, err := skills.Get(ctx, "learned-thing")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "v2" {
		t.Fatalf("learned skill body = %q, want v2", got.Body)
	}
}

func TestSlugifyProducesConformantNames(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"basic", "Reset the Widget", "reset-the-widget"},
		{"runs collapse", "a  --  b", "a-b"},
		{"trims ends", "  !Hello!  ", "hello"},
		{"empty falls back", "!!!", "skill"},
		{"caps at the name limit", strings.Repeat("word ", 40), strings.Repeat("word-", 12) + "word"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slugify(tc.title)
			if got != tc.want {
				t.Errorf("slugify(%q) = %q, want %q", tc.title, got, tc.want)
			}
			// Whatever slugify returns must be a valid Agent Skills name, so a
			// learned skill is always exportable.
			if err := skillmd.ValidateName(got); err != nil {
				t.Errorf("slugify(%q) = %q, not a conformant name: %v", tc.title, got, err)
			}
		})
	}
}

func TestSlugifyIsAlwaysAConformantName(t *testing.T) {
	titles := []string{
		"", " ", "---", "a", strings.Repeat("x", 200),
		"CAPS and Ünïcodé mixed 12345", strings.Repeat("a-", 50),
		"trailing hyphen after cut " + strings.Repeat("z", 60) + " a",
	}
	for _, title := range titles {
		got := slugify(title)
		if err := skillmd.ValidateName(got); err != nil {
			t.Errorf("slugify(%q) = %q, not a conformant name: %v", title, got, err)
		}
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

// A lesson carries the provenance of the outcome it was distilled from. An outcome
// that recorded no source produces an item with no provenance rather than one
// sourced to the empty string, which the guard would otherwise have to grade.
func TestSourcesOfDropsAnUnrecordedSource(t *testing.T) {
	if got := sourcesOf(""); got != nil {
		t.Errorf("sourcesOf(\"\") = %v, want no sources at all", got)
	}
	if got := sourcesOf("run-1"); !slices.Equal(got, []string{"run-1"}) {
		t.Errorf("sourcesOf(run-1) = %v, want [run-1]", got)
	}
}

// A memory lesson is anchored to the skills the run loaded, so the next read of one
// of those procedures surfaces what was learned while working from it. Without the
// anchor the ride-along has nothing to match on however well it is wired, which is
// why this is written at the capture end rather than left to a caller.
func TestCuratorAnchorsMemoryToTheSkillsTheRunRead(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{
		{Kind: LessonMemory, Body: "The firmware reset needs the case open."},
		{Kind: LessonSkill, Title: "Reset the Widget", Body: "Hold for 10s."},
	}}
	c := NewCurator(d, skills, memories)
	ctx := context.Background()

	o := convergedOutcome()
	o.SkillsRead = []string{"sk-2", "sk-1", ""}
	captured, err := c.Curate(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Memories) != 1 {
		t.Fatalf("captured %d memories, want 1", len(captured.Memories))
	}
	// Canonical order, and the empty id dropped: the store normalizes what it is
	// given, and a list of read ids is not the caller's to clean up first.
	want := []state.Anchor{{Kind: state.AnchorKindSkill, ID: "sk-1"}, {Kind: state.AnchorKindSkill, ID: "sk-2"}}
	if got := captured.Memories[0].Anchors; !slices.Equal(got, want) {
		t.Errorf("memory anchors = %v, want %v", got, want)
	}

	// Recallable by the anchor, which is the whole point of writing it.
	items, err := memories.Recall(ctx, state.RecallQuery{Anchors: []state.Anchor{state.SkillAnchor("sk-1")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "The firmware reset needs the case open." {
		t.Fatalf("recall by skill anchor = %+v", items)
	}
}

// A run that loaded no skill anchors nothing. The alternative would be an anchor to
// whatever else was to hand, and a memory anchored to something it is not about is
// worse than one a reader has to search for.
func TestCuratorAnchorsNothingWhenNoSkillWasRead(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{{Kind: LessonMemory, Body: "A fact from a run that read nothing."}}}
	captured, err := NewCurator(d, skills, memories).Curate(context.Background(), convergedOutcome())
	if err != nil {
		t.Fatal(err)
	}
	if got := captured.Memories[0].Anchors; got != nil {
		t.Errorf("memory anchors = %v, want none", got)
	}
}
