package skilltool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
)

// pack is a skill tree with a resource in it. The bundled pack ships empty, so a
// test that wants to prove a resource is readable has to bring its own tree; the
// loader is the same one either way.
func pack() fstest.MapFS {
	return fstest.MapFS{
		"skills/tidy-diff/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: tidy-diff\ndescription: Reduce a change to the smallest diff that still does the job, before asking anyone to read it.\n---\n\nRead the diff as a reviewer would.\nSee references/checklist.md for the pass order.\n")},
		"skills/tidy-diff/references/checklist.md": &fstest.MapFile{Data: []byte("1. Delete what the change does not need.\n")},
		"skills/tidy-diff/scripts/run.sh":          &fstest.MapFile{Data: []byte("#!/bin/sh\nexit 0\n")},
	}
}

// setup returns a toolset over a store holding sk, reading resources from pack.
func setup(t *testing.T, sk state.Skill) (map[string]mission.Tool, state.SkillStore) {
	t.Helper()
	skills := state.NewMemory().Skills()
	if _, err := skills.Upsert(context.Background(), sk); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	tools := map[string]mission.Tool{}
	for _, tool := range skilltool.New(skills, skilltool.WithPack(pack(), "skills")).Tools() {
		tools[tool.Def().Name] = tool
	}
	return tools, skills
}

// bundledSkill is the stored form of the pack's one skill: the bundled scope, so the
// resource path resolves, and a body long enough that truncating it would show.
func bundledSkill() state.Skill {
	return state.Skill{
		Slug:        "tidy-diff",
		Name:        "tidy-diff",
		Description: "Reduce a change to the smallest diff that still does the job.",
		Body:        "Read the diff as a reviewer would.\n" + strings.Repeat("Then remove what the change does not need. ", 40),
		Scope:       state.BundledScope,
	}
}

func call(t *testing.T, tool mission.Tool, args any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Invoke(context.Background(), b)
}

// TestReadReturnsTheWholeBody is the point of the task: the body reaches the model
// entire. Recall carries a description; anything that clipped the procedure here
// would put the model back where it started, acting on half a rule.
func TestReadReturnsTheWholeBody(t *testing.T) {
	sk := bundledSkill()
	tools, _ := setup(t, sk)

	got, err := call(t, tools["skill_read"], map[string]string{"skill": "tidy-diff"})
	if err != nil {
		t.Fatalf("skill_read: %v", err)
	}
	if !strings.Contains(got, strings.TrimSpace(sk.Body)) {
		t.Errorf("skill_read did not return the whole body, got %d chars for a %d-char body", len(got), len(sk.Body))
	}
	// The addressable set is named, so the model knows what it may ask for next.
	for _, want := range []string{"references/checklist.md", "scripts/run.sh"} {
		if !strings.Contains(got, want) {
			t.Errorf("skill_read did not list %s among the skill's resources", want)
		}
	}
}

// TestReadRefusesRatherThanTruncates locks the refusal. A body over the cap is a
// malformed skill, and saying so is better than handing over a procedure that stops
// mid-sentence with nothing to say it did.
func TestReadRefusesRatherThanTruncates(t *testing.T) {
	sk := bundledSkill()
	sk.Body = strings.Repeat("x", skilltool.MaxBodyRunes+1)
	tools, _ := setup(t, sk)

	got, err := call(t, tools["skill_read"], map[string]string{"skill": "tidy-diff"})
	if !errors.Is(err, skilltool.ErrUnreadable) {
		t.Fatalf("skill_read of an oversized body: err = %v, want ErrUnreadable", err)
	}
	if got != "" {
		t.Errorf("skill_read returned %d characters alongside its error; a refusal returns nothing", len(got))
	}

	// The other end of the same rule: a skill with nothing to say says so, rather than
	// returning the empty string the model would read as a procedure of no steps.
	empty := bundledSkill()
	empty.Body = "   \n"
	emptyTools, _ := setup(t, empty)
	if _, err := call(t, emptyTools["skill_read"], map[string]string{"skill": "tidy-diff"}); !errors.Is(err, skilltool.ErrUnreadable) {
		t.Errorf("skill_read of an empty body: err = %v, want ErrUnreadable", err)
	}
}

// TestUnknownSkillIsTyped keeps the wrong-name case distinguishable from the
// broken-record case, which is what lets a caller tell the model to try another name
// rather than reporting the skill as unusable.
func TestUnknownSkillIsTyped(t *testing.T) {
	tools, _ := setup(t, bundledSkill())
	for _, name := range []string{"no-such-skill", "", "  "} {
		if _, err := call(t, tools["skill_read"], map[string]string{"skill": name}); !errors.Is(err, skilltool.ErrUnknownSkill) {
			t.Errorf("skill_read(%q): err = %v, want ErrUnknownSkill", name, err)
		}
	}
}

// TestResourceReadsOnlyWhatTheSkillAddresses is the access rule. Membership of the
// loader's own list is the whole check, so a path outside the skill is refused
// without any reasoning about what the string might have meant.
func TestResourceReadsOnlyWhatTheSkillAddresses(t *testing.T) {
	tools, _ := setup(t, bundledSkill())

	got, err := call(t, tools["skill_resource"], map[string]string{"skill": "tidy-diff", "path": "references/checklist.md"})
	if err != nil {
		t.Fatalf("skill_resource: %v", err)
	}
	if !strings.Contains(got, "Delete what the change does not need") {
		t.Errorf("skill_resource returned %q, not the resource's contents", got)
	}

	for _, path := range []string{
		"SKILL.md",                    // in the directory, not a resource
		"../tidy-diff/scripts/run.sh", // the same file by a path that leaves the skill
		"references/absent.md",        // addressed by nothing
		"/etc/passwd",                 // absolute
	} {
		if _, err := call(t, tools["skill_resource"], map[string]string{"skill": "tidy-diff", "path": path}); !errors.Is(err, skilltool.ErrNoResource) {
			t.Errorf("skill_resource(%q): err = %v, want ErrNoResource", path, err)
		}
	}
}

// TestResourcesAreBundledOnly records the limit honestly: a skill this install
// learned has no tree to read from, so it addresses nothing rather than reading
// something a pack of the same slug happens to ship.
func TestResourcesAreBundledOnly(t *testing.T) {
	sk := bundledSkill()
	sk.Scope = state.Scope{}
	tools, _ := setup(t, sk)

	got, err := call(t, tools["skill_read"], map[string]string{"skill": "tidy-diff"})
	if err != nil {
		t.Fatalf("skill_read: %v", err)
	}
	if strings.Contains(got, "checklist.md") {
		t.Error("a learned skill was offered the bundled pack's resources")
	}
	if _, err := call(t, tools["skill_resource"], map[string]string{"skill": "tidy-diff", "path": "references/checklist.md"}); !errors.Is(err, skilltool.ErrNoResource) {
		t.Errorf("skill_resource on a learned skill: err = %v, want ErrNoResource", err)
	}
}

// TestMissingPackIsSaidOutLoud covers the store holding a bundled skill this binary
// no longer ships, which a downgrade produces. The procedure is still the store's to
// give, so the read succeeds and says what it could not list; asking for one of the
// resources it could not list is refused.
func TestMissingPackIsSaidOutLoud(t *testing.T) {
	sk := bundledSkill()
	sk.Slug, sk.Name = "gone-from-this-binary", "gone-from-this-binary"
	tools, _ := setup(t, sk)

	got, err := call(t, tools["skill_read"], map[string]string{"skill": sk.Slug})
	if err != nil {
		t.Fatalf("skill_read: %v", err)
	}
	if !strings.Contains(got, strings.TrimSpace(sk.Body)) {
		t.Error("skill_read withheld a body it had, over a pack directory it did not")
	}
	if !strings.Contains(got, "cannot be listed") {
		t.Errorf("skill_read passed over the missing pack in silence:\n%s", got)
	}
	if _, err := call(t, tools["skill_resource"], map[string]string{"skill": sk.Slug, "path": "references/checklist.md"}); !errors.Is(err, skilltool.ErrNoResource) {
		t.Errorf("skill_resource against a missing pack: err = %v, want ErrNoResource", err)
	}
}

// failingStore answers every read with the same store failure, which is what a
// broken database looks like from here.
type failingStore struct{ state.SkillStore }

var errStoreDown = errors.New("store is down")

func (failingStore) Get(context.Context, string) (state.Skill, error) {
	return state.Skill{}, errStoreDown
}

// TestBadInputAndBrokenStoreSurface keeps the two failures that are nobody's fault
// distinguishable from the ones that are. Malformed arguments and a store that will
// not answer both come back as errors rather than as an empty result the model would
// read as "this skill says nothing".
func TestBadInputAndBrokenStoreSurface(t *testing.T) {
	tools, _ := setup(t, bundledSkill())
	for name, tool := range tools {
		if _, err := tool.Invoke(context.Background(), json.RawMessage(`{`)); err == nil {
			t.Errorf("%s accepted malformed arguments", name)
		}
	}

	down := map[string]mission.Tool{}
	for _, tool := range skilltool.New(failingStore{}).Tools() {
		down[tool.Def().Name] = tool
	}
	for name, tool := range down {
		_, err := call(t, tool, map[string]string{"skill": "tidy-diff", "path": "references/checklist.md"})
		if !errors.Is(err, errStoreDown) {
			t.Errorf("%s over a broken store: err = %v, want the store's own error", name, err)
		}
	}
}

// TestNoStoreOffersNoTools keeps the offer honest at the other end: a run with no
// durable skills behind it advertises no tool that could only fail.
func TestNoStoreOffersNoTools(t *testing.T) {
	if got := skilltool.New(nil).Tools(); len(got) != 0 {
		t.Errorf("a toolset with no store offered %d tools, want none", len(got))
	}
}

// TestReadsRecordWhatTheRunLoaded covers the fact the grading loop is credited
// against. The set is the run's record: a body handed over is a read, a repeat is
// the same read, and the id is what comes back, because a slug can name two records
// in two scopes and the run loaded exactly one of them.
func TestReadsRecordWhatTheRunLoaded(t *testing.T) {
	skills := state.NewMemory().Skills()
	ctx := context.Background()
	first, err := skills.Upsert(ctx, bundledSkill())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, err := skills.Upsert(ctx, state.Skill{Slug: "other", Name: "other", Body: "another procedure"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	set := skilltool.New(skills, skilltool.WithPack(pack(), "skills"))
	if got := set.Reads(); len(got) != 0 {
		t.Fatalf("a set that served nothing reports %v, want no reads", got)
	}

	var read mission.Tool
	for _, tool := range set.Tools() {
		if tool.Def().Name == "skill_read" {
			read = tool
		}
	}
	for _, name := range []string{"other", "tidy-diff", "other"} {
		if _, err := call(t, read, map[string]string{"skill": name}); err != nil {
			t.Fatalf("skill_read %s: %v", name, err)
		}
	}
	got := set.Reads()
	want := []string{second.ID, first.ID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("reads = %v, want %v (deduped, first-read order, by id)", got, want)
	}
}

// TestARefusedReadIsNotARead is what keeps the counter honest at its own boundary.
// skill_read refuses a name it cannot resolve and a body it cannot hand over whole;
// in both cases no procedure reached the model, so crediting the run's outcome to
// the skill would be the same overclaim as crediting it to an offer.
func TestARefusedReadIsNotARead(t *testing.T) {
	empty := bundledSkill()
	empty.Slug, empty.Name, empty.Body = "hollow", "hollow", "   "
	skills := state.NewMemory().Skills()
	if _, err := skills.Upsert(context.Background(), empty); err != nil {
		t.Fatalf("seed: %v", err)
	}
	set := skilltool.New(skills, skilltool.WithPack(pack(), "skills"))
	var read mission.Tool
	for _, tool := range set.Tools() {
		if tool.Def().Name == "skill_read" {
			read = tool
		}
	}

	for _, name := range []string{"hollow", "no-such-skill"} {
		if _, err := call(t, read, map[string]string{"skill": name}); err == nil {
			t.Fatalf("skill_read %s returned no error, want a refusal", name)
		}
	}
	if got := set.Reads(); len(got) != 0 {
		t.Fatalf("reads = %v after two refusals, want none", got)
	}
}
