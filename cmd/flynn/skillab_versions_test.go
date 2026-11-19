package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/skill/skillab"
)

func TestValidRefAllowsOnlyAPlainRefAndRefusesAShellLine(t *testing.T) {
	for ref, ok := range map[string]bool{
		"main":                   true,
		"v1.2.3":                 true,
		"origin/feat-x":          true,
		"3f9c2ab":                true,
		"":                       false,
		"-rf":                    false,
		"a b":                    false,
		"main;rm":                false,
		"main$(id)":              false,
		"a|b":                    false,
		"a`b`":                   false,
		"main..other":            false,
		"../x":                   false,
		"main\nrm":               false,
		strings.Repeat("a", 201): false,
	} {
		if err := validRef(ref); (err == nil) != ok {
			t.Errorf("validRef(%q) = %v, want ok=%v", ref, err, ok)
		}
	}
}

type fakeModel struct {
	replies []llm.Response
	err     map[int]error
	n       int
}

func (f *fakeModel) Generate(context.Context, llm.Request) (llm.Response, error) {
	i := f.n
	f.n++
	if err := f.err[i]; err != nil {
		return llm.Response{}, err
	}
	return f.replies[i], nil
}

func reply(text string, in, out int) llm.Response {
	return llm.Response{Message: llm.Text(llm.RoleAssistant, text), Usage: llm.Usage{InputTokens: in, OutputTokens: out}}
}

// The loop prunes and compacts the history it resends, so a request is not a record of
// the run. The transcript has to be the first request plus what each later turn added.
func TestMeteredModelBuildsTheTranscriptTurnByTurn(t *testing.T) {
	m := newMeteredModel(&fakeModel{
		replies: []llm.Response{reply("first", 10, 1), {}, reply("second", 20, 2)},
		err:     map[int]error{1: errors.New("rate limited")},
	})
	ctx := context.Background()
	user := llm.Text(llm.RoleUser, "fix it")
	if _, err := m.Generate(ctx, llm.Request{Messages: []llm.Message{user}}); err != nil {
		t.Fatal(err)
	}
	// The history was compacted: the earlier turns are gone, only the new message remains.
	obs := llm.Text(llm.RoleUser, "observation")
	if _, err := m.Generate(ctx, llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "[summary]"), obs}}); err == nil {
		t.Fatal("expected the scripted failure")
	}
	if _, err := m.Generate(ctx, llm.Request{Messages: []llm.Message{llm.Text(llm.RoleUser, "[summary]"), obs}}); err != nil {
		t.Fatal(err)
	}

	cost, log, last := m.snapshot()
	if cost.Turns != 2 || cost.InputTokens != 30 || cost.OutputTokens != 3 {
		t.Errorf("cost = %+v, want 2 turns, 30 in, 3 out: a failed call reported no usage and counts for nothing", cost)
	}
	if last != "second" {
		t.Errorf("last = %q", last)
	}
	for _, want := range []string{"fix it", "first", "observation", "second", "call failed"} {
		if !strings.Contains(log, want) {
			t.Errorf("transcript lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "[summary]") && strings.Count(log, "fix it") != 1 {
		t.Errorf("an earlier message was recorded again:\n%s", log)
	}
}

func TestMeteredModelBoundsOneBlock(t *testing.T) {
	m := newMeteredModel(&fakeModel{replies: []llm.Response{reply(strings.Repeat("x", 3*maxLoggedBlock), 1, 1)}})
	if _, err := m.Generate(context.Background(), llm.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, log, _ := m.snapshot(); len(log) > 2*maxLoggedBlock {
		t.Errorf("transcript is %d bytes for one long block", len(log))
	}
}

func versionDir(t *testing.T, slug, body, ref string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o750); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + slug + "\ndescription: a skill used to test versions\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "r.md"), []byte(ref), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A baseline that read the candidate's references would not be a baseline. Each
// version carries its own tree.
func TestResolveVersionKeepsEachVersionsReferencesApart(t *testing.T) {
	ctx := context.Background()
	a, err := resolveVersion(ctx, "demo", versionDir(t, "demo", "old body", "old reference"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	b, err := resolveVersion(ctx, "demo", versionDir(t, "demo", "new body", "new reference"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()

	read := func(v *skillVersion) string {
		raw, err := os.ReadFile(filepath.Join(v.dir, v.root, "demo", "references", "r.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if read(a) != "old reference" || read(b) != "new reference" {
		t.Errorf("versions share a tree: %q and %q", read(a), read(b))
	}
	if a.skill.Slug != "demo" || !strings.Contains(a.skill.Body, "old body") || !strings.Contains(b.skill.Body, "new body") {
		t.Errorf("bodies: %q, %q", a.skill.Body, b.skill.Body)
	}
	if a.label == b.label && a.dir == b.dir {
		t.Error("two versions are indistinguishable")
	}
}

func TestResolveVersionRefusesWhatIsNeitherAPathNorARef(t *testing.T) {
	for _, ref := range []string{"no such; thing", "$(id)", "../elsewhere"} {
		if _, err := resolveVersion(context.Background(), "demo", ref); err == nil {
			t.Errorf("resolved %q", ref)
		}
	}
	other := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveVersion(context.Background(), "demo", other); err == nil {
		t.Error("a file that is not a SKILL.md named a version")
	}
}

func TestSkillABArgsReadsBothVersions(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "open-only"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "open-only", skillab.ExercisesFile), []byte("do it | exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, _, refs, err := skillABArgs([]string{"--exercises", root, "open-only", "--candidate", "feat/x", "--against", "main"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if refs.candidate != "feat/x" || refs.against != "main" {
		t.Errorf("refs = %+v", refs)
	}
}
