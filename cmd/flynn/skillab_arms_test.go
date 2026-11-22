package main

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/state"
)

// gitRepoWithSkill makes a repository whose one commit holds a demo skill at the path
// the binary's own pack uses, and moves the test into it, because a git ref is read
// from the repository in the current directory.
func gitRepoWithSkill(t *testing.T, body, ref string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	src := versionDir(t, "demo", body, ref)
	if err := os.CopyFS(filepath.Join(repo, "skill", "bundled", bundled.Root, "demo"), os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "demo"},
		{"tag", "v-demo"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repo)
}

// A ref is read from git, file by file, so the baseline's references are the ones
// committed at that ref and not whatever the working tree holds now.
func TestResolveVersionReadsAGitRef(t *testing.T) {
	gitRepoWithSkill(t, "committed body", "committed reference")
	if err := os.WriteFile(filepath.Join("skill", "bundled", bundled.Root, "demo", "references", "r.md"), []byte("edited since"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := resolveVersion(context.Background(), "demo", "v-demo")
	if err != nil {
		t.Fatal(err)
	}
	defer v.close()
	raw, err := os.ReadFile(filepath.Join(v.dir, v.root, "demo", "references", "r.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "committed reference" {
		t.Errorf("reference = %q, want the committed one", raw)
	}
	if !strings.Contains(v.skill.Body, "committed body") || v.label != "demo @ v-demo" {
		t.Errorf("version = %q, body %q", v.label, v.skill.Body)
	}
}

func TestReadGitTreeRefusesWhatItCannotRead(t *testing.T) {
	gitRepoWithSkill(t, "body", "reference")
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "out", "demo")
	for name, c := range map[string]struct{ ref, dir, want string }{
		"unknown ref":       {"no-such-ref", path.Join("skill", "bundled", bundled.Root, "demo"), "could not list"},
		"no skill at a ref": {"v-demo", path.Join("skill", "bundled", bundled.Root, "absent"), "has no"},
	} {
		err := readGitTree(ctx, c.ref, c.dir, dest)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, c.want)
		}
	}
}

// The candidate replaces the pack's copy of its slug under the same ID, so a trial
// offers one version of the skill and whatever refers to the shipped copy by ID now
// reaches the version under test.
func TestInstallReplacesTheShippedCopy(t *testing.T) {
	ctx := context.Background()
	store, err := openDataStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	plantBundledSkill(ctx, t, store.Skills(), "demo")
	shipped, err := store.Skills().Get(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}

	v, err := resolveVersion(ctx, "demo", versionDir(t, "demo", "candidate body", "r"))
	if err != nil {
		t.Fatal(err)
	}
	defer v.close()
	if err := v.install(ctx, store.Skills()); err != nil {
		t.Fatal(err)
	}
	list, err := store.Skills().List(ctx, state.BundledScope)
	if err != nil {
		t.Fatal(err)
	}
	var got []state.Skill
	for _, sk := range list {
		if sk.Slug == "demo" {
			got = append(got, sk)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Body, "candidate body") {
		t.Fatalf("demo in the store = %+v, want only the candidate", got)
	}
	if got[0].ID != shipped.ID {
		t.Errorf("installed under ID %v, want the shipped copy's %v", got[0].ID, shipped.ID)
	}
}

func TestResolveArmsNamesWhatEachArmRuns(t *testing.T) {
	ctx := context.Background()
	slug := "demo"
	zero, err := resolveArms(ctx, slug, versionRefs{})
	if err != nil || zero.candidate != nil || zero.baseline != nil || zero.names != (abArms{}).names {
		t.Errorf("no versions named: %+v, %v; want the skill against its absence", zero, err)
	}

	cand := versionDir(t, slug, "new", "r")
	onlyCandidate, err := resolveArms(ctx, slug, versionRefs{candidate: cand})
	if err != nil {
		t.Fatal(err)
	}
	defer onlyCandidate.close()
	if onlyCandidate.baseline != nil || onlyCandidate.names.Baseline != "without the skill" || onlyCandidate.names.Candidate != slug+" @ "+cand {
		t.Errorf("candidate only: %+v", onlyCandidate.names)
	}

	against := filepath.Join(versionDir(t, slug, "old", "r"), "SKILL.md")
	onlyAgainst, err := resolveArms(ctx, slug, versionRefs{against: against})
	if err != nil {
		t.Fatal(err)
	}
	defer onlyAgainst.close()
	if onlyAgainst.candidate != nil || onlyAgainst.names.Candidate != slug+" as installed" || onlyAgainst.names.Baseline != slug+" @ "+against {
		t.Errorf("against only: %+v", onlyAgainst.names)
	}

	if _, err := resolveArms(ctx, slug, versionRefs{candidate: cand, against: "no such; thing"}); err == nil {
		t.Error("a baseline that cannot be read was accepted")
	}
	if _, err := resolveArms(ctx, slug, versionRefs{candidate: "$(id)"}); err == nil {
		t.Error("a candidate that cannot be read was accepted")
	}
}
