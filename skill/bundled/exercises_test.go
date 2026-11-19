package bundled

import (
	"context"
	"encoding/base64"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skillab"
)

// evalRoot holds one exercise set per skill. It sits outside the pack so the run being
// measured cannot read the checks through skill_resource.
const evalRoot = "../../evals"

// minExercises is the smallest set that can separate two arms at all.
const minExercises = 3

// verifierTimeout bounds one verifier run; a hung check must fail the build, not stall it.
const verifierTimeout = 90 * time.Second

// readSlugList reads testdata/<file>: one `slug reason` per line, `#` comments.
func readSlugList(t *testing.T, file string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if h := strings.Index(line, "#"); h >= 0 {
			line = line[:h]
		}
		slug, reason, _ := strings.Cut(strings.TrimSpace(line), " ")
		if slug == "" {
			continue
		}
		out[slug] = strings.TrimSpace(reason)
	}
	return out
}

func bundledSlugs(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(FS(), Root)
	if err != nil {
		t.Fatalf("list the pack: %v", err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() {
			slugs = append(slugs, e.Name())
		}
	}
	return slugs
}

// A skill that ships without an exercise set is unmeasured, and the instrument exists
// to measure it. The exemption list is the backlog: it may only shrink, so a new
// skill arrives with its set or this test fails, and an exemption for a skill that
// has since gained a set is refused so the list cannot go stale.
func TestEveryBundledSkillHasAnExerciseSet(t *testing.T) {
	exempt := readSlugList(t, "exercise_exempt.txt")
	slugs := bundledSlugs(t)

	for slug := range exempt {
		if !slices.Contains(slugs, slug) {
			t.Errorf("exercise_exempt.txt names %q, which is not a bundled skill: delete the line", slug)
		}
	}
	for _, slug := range slugs {
		set, err := skillab.LoadDir(filepath.Join(evalRoot, slug), slug)
		_, isExempt := exempt[slug]
		switch {
		case err != nil && isExempt:
		case err != nil:
			t.Errorf("bundled skill %q has no usable exercise set: %v\nwrite evals/%s/exercises.txt; the exemption list is for the existing backlog only", slug, err, slug)
		case isExempt:
			t.Errorf("%q has an exercise set now: delete its line from testdata/exercise_exempt.txt", slug)
		case len(set.Exercises) < minExercises:
			t.Errorf("%q has %d exercises; a set under %d cannot separate two arms", slug, len(set.Exercises), minExercises)
		}
	}

	for slug := range readSlugList(t, "exercise_unproven.txt") {
		if _, err := skillab.LoadDir(filepath.Join(evalRoot, slug), slug); err != nil {
			t.Errorf("exercise_unproven.txt names %q, which has no exercise set: delete the line", slug)
		}
	}

	dirs, err := os.ReadDir(evalRoot)
	if err != nil {
		t.Fatalf("read %s: %v", evalRoot, err)
	}
	for _, d := range dirs {
		if d.IsDir() && !slices.Contains(slugs, d.Name()) {
			t.Errorf("evals/%s is not a bundled skill: a renamed or removed skill leaves its set measuring nothing", d.Name())
		}
	}
}

// verifierForm is the only verifier shape the sets use: the logic is Python read from a
// base64 argument, so no quoting rule of sh or cmd.exe can change what it means, and
// `#` and `|`, which the loader treats as comment and column, cannot cut it short.
var verifierForm = regexp.MustCompile(`^python3 -c "import base64;exec\(base64\.b64decode\('([A-Za-z0-9+/=]+)'\)\)"$`)

// encodeVerifier is the line a set holds for the Python source src.
func encodeVerifier(src string) string {
	return `python3 -c "import base64;exec(base64.b64decode('` + base64.StdEncoding.EncodeToString([]byte(src)) + `'))"`
}

func normalise(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n") + "\n"
}

// A verifier nobody can read is a verifier nobody reviews. Each row's Python source is
// kept readable in testdata/exercise_verifiers and the row must be exactly its
// encoding, so editing one without the other fails here and prints the line to paste.
func TestExerciseVerifiersMatchTheirSource(t *testing.T) {
	forEachExercise(t, func(t *testing.T, slug string, e skillab.Exercise) {
		if !verifierForm.MatchString(e.Verify) {
			t.Errorf("%s: verifier must be `python3 -c \"import base64;exec(base64.b64decode('...'))\"`", exerciseLabel(slug, e))
			return
		}
		src, err := os.ReadFile(filepath.Join("testdata", "exercise_verifiers", slug, e.Fixture+".py"))
		if err != nil {
			t.Errorf("%s: the verifier's source is missing: %v", exerciseLabel(slug, e), err)
			return
		}
		if want := encodeVerifier(normalise(string(src))); e.Verify != want {
			t.Errorf("%s: exercises.txt does not hold the encoding of testdata/exercise_verifiers/%s/%s.py; the row should end with:\n%s",
				exerciseLabel(slug, e), slug, e.Fixture, want)
		}
		if len(e.Verify) > 7000 {
			t.Errorf("%s: verifier is %d bytes; cmd.exe stops at 8191", exerciseLabel(slug, e), len(e.Verify))
		}
	})
}

// Every row needs a fixture, because an exercise here starts from a working tree.
func TestExercisesHaveFixtures(t *testing.T) {
	forEachExercise(t, func(t *testing.T, slug string, e skillab.Exercise) {
		if e.Fixture == "" {
			t.Errorf("%s: no fixture", exerciseLabel(slug, e))
		}
	})
}

// A check that cannot fail on a wrong answer proves nothing, and one that cannot pass
// on a right answer measures nothing. Each verifier is run through the sandbox that
// `flynn skill ab` uses, against three trees: the fixture as shipped, a reference
// solution from testdata/exercise_solutions, and each tempting wrong answer from
// testdata/exercise_wrong. A fixture with no solution is an exercise whose right
// answer is to change nothing: the shipped tree must pass, and at least one wrong
// answer must exist to fail it.
func TestExerciseVerifiersTellAnswersApart(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH: the verifiers need it, so nothing here was checked")
	}
	testdata := os.DirFS("testdata")
	forEachExercise(t, func(t *testing.T, slug string, e skillab.Exercise) {
		t.Parallel() // each verifier run starts a sandbox; sequentially they dominate the package's time
		set, err := skillab.LoadDir(filepath.Join(evalRoot, slug), slug)
		if err != nil {
			t.Fatal(err)
		}
		solution := path.Join("exercise_solutions", slug, e.Fixture)
		_, serr := fs.Stat(testdata, solution)
		hasSolution := serr == nil

		verdict := func(overlayDir string) bool {
			work := t.TempDir()
			if err := set.Materialise(e, work); err != nil {
				t.Fatalf("%s: %v", exerciseLabel(slug, e), err)
			}
			if overlayDir != "" {
				overlay(t, testdata, overlayDir, work)
			}
			return passes(t, work, e.Verify)
		}

		pristine := verdict("")
		if hasSolution {
			if pristine {
				t.Errorf("%s: the verifier passes on the fixture as shipped, so the solution in %s is not what it grades", exerciseLabel(slug, e), solution)
			}
			if !verdict(solution) {
				t.Errorf("%s: the verifier fails on the reference solution %s", exerciseLabel(slug, e), solution)
			}
		} else if !pristine {
			t.Errorf("%s: no solution overlay and the fixture as shipped fails: add testdata/%s, or fix the verifier", exerciseLabel(slug, e), solution)
		}

		wrongRoot := path.Join("exercise_wrong", slug, e.Fixture)
		variants, _ := fs.ReadDir(testdata, wrongRoot)
		if !hasSolution && len(variants) == 0 {
			t.Errorf("%s: the right answer is to change nothing and no wrong answer is given, so the verifier has never been seen to fail: add testdata/%s/<variant>", exerciseLabel(slug, e), wrongRoot)
		}
		for _, v := range variants {
			if v.IsDir() && verdict(path.Join(wrongRoot, v.Name())) {
				t.Errorf("%s: the verifier passes the wrong answer %s/%s", exerciseLabel(slug, e), wrongRoot, v.Name())
			}
		}
	})
}

// An overlay for an exercise that no longer exists would keep passing while checking
// nothing.
func TestExerciseOverlaysBelongToAnExercise(t *testing.T) {
	known := map[string]bool{}
	forEachExercise(t, func(_ *testing.T, slug string, e skillab.Exercise) { known[slug+"/"+e.Fixture] = true })
	for _, kind := range []string{"exercise_solutions", "exercise_wrong"} {
		skills, err := os.ReadDir(filepath.Join("testdata", kind))
		if err != nil {
			continue
		}
		for _, s := range skills {
			fixtures, _ := os.ReadDir(filepath.Join("testdata", kind, s.Name()))
			for _, f := range fixtures {
				if !known[s.Name()+"/"+f.Name()] {
					t.Errorf("testdata/%s/%s/%s matches no exercise: delete it or fix the name", kind, s.Name(), f.Name())
				}
			}
		}
	}
}

func exerciseLabel(slug string, e skillab.Exercise) string {
	return slug + "/" + e.Fixture
}

// forEachExercise runs fn once per exercise of every bundled skill that has a set and
// is not on the unproven list.
func forEachExercise(t *testing.T, fn func(t *testing.T, slug string, e skillab.Exercise)) {
	t.Helper()
	unproven := readSlugList(t, "exercise_unproven.txt")
	for _, slug := range bundledSlugs(t) {
		if _, skip := unproven[slug]; skip {
			continue
		}
		set, err := skillab.LoadDir(filepath.Join(evalRoot, slug), slug)
		if err != nil {
			continue
		}
		for _, e := range set.Exercises {
			t.Run(slug+"/"+e.Fixture, func(t *testing.T) { fn(t, slug, e) })
		}
	}
}

// overlayDeletes names, one path per line, files an overlay removes from the tree it is
// laid over; a copy cannot express a deletion on its own.
const overlayDeletes = ".delete"

// overlay copies the tree at dir in src over dest, so a solution or a wrong answer is
// only the files it changes, then removes the paths its .delete file lists.
func overlay(t *testing.T, src fs.FS, dir, dest string) {
	t.Helper()
	err := fs.WalkDir(src, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		if rel == overlayDeletes {
			for _, gone := range strings.Fields(string(data)) {
				if err := os.Remove(filepath.Join(dest, filepath.FromSlash(gone))); err != nil {
					return err
				}
			}
			return nil
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("overlay %s: %v", dir, err)
	}
}

// passes runs line in dir the way a trial's verifier is run.
func passes(t *testing.T, dir, line string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), verifierTimeout)
	defer cancel()
	sb, err := sandbox.NewLocal(dir, sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	defer func() { _ = sb.Close() }()
	res, err := sb.Exec(ctx, sandbox.Command{Line: line})
	if err != nil {
		t.Fatalf("verifier did not run: %v", err)
	}
	return res.ExitCode == 0
}
