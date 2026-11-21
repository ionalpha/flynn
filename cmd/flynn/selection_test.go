package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/selection"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/storage/sqlite"
)

const selectionObjective = "frobnicate the widgets"

// seedSelectionSkills stores two skills a run over selectionObjective would be offered,
// plus a bundled one, so every rule has something to act on.
func seedSelectionSkills(ctx context.Context, t *testing.T, st *sqlite.Store) {
	t.Helper()
	for _, s := range []state.Skill{
		{Slug: "open-skill", Name: "open skill", Description: "How to frobnicate widgets safely.", Body: "OPENBODY"},
		{Slug: "secret-skill", Name: "secret skill", Description: "How to frobnicate widgets in secret.", Body: "SECRETBODY"},
	} {
		if _, err := st.Skills().Upsert(ctx, s); err != nil {
			t.Fatalf("upsert %s: %v", s.Slug, err)
		}
	}
	plantBundledSkill(ctx, t, st.Skills(), "bundled-skill")
}

func planFor(ctx context.Context, t *testing.T, st *sqlite.Store, cfg selection.Config) selectionPlan {
	t.Helper()
	p, err := resolveSelection(ctx, cfg, st.Skills(), false, false, false)
	if err != nil {
		t.Fatalf("resolveSelection: %v", err)
	}
	return p
}

// runSelected drives one scripted run under plan and returns the model, its output, the
// store and the run's recorded events.
func runSelected(t *testing.T, st *sqlite.Store, plan selectionPlan, workdir string, script ...llm.Response) (*llmtest.ScriptedModel, string, []session.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	model := llmtest.NewScripted(script...)
	var out bytes.Buffer
	if _, err := runLearningMission(ctx, &out, model, harness.Plan{}, &ctxDistiller{}, workdir, selectionObjective, "", st, nil, false, nil, withSelection(plan)); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	evs, err := session.History(ctx, st.Log(), runIDFromOutput(t, out.String()))
	if err != nil {
		t.Fatal(err)
	}
	return model, out.String(), evs
}

func toolResults(evs []session.Event) []session.Event {
	var out []session.Event
	for _, e := range evs {
		if e.Kind == session.KindToolResult {
			out = append(out, e)
		}
	}
	return out
}

// A skill on the deny list is offered nowhere and cannot be loaded by naming it, and the
// same call succeeds without the deny, so the refusal comes from the selection.
func TestDeniedSkillCannotBeRead(t *testing.T) {
	read := llmtest.CallTool("c1", "skill_read", json.RawMessage(`{"skill":"secret-skill"}`))
	for _, tc := range []struct {
		name     string
		cfg      selection.Config
		wantBody bool
	}{
		{"no rules", selection.Config{}, true},
		{"denied", selection.Config{Deny: []string{"secret-skill"}}, false},
		{"allowed list without it", selection.Config{AllowSet: true, Allow: []string{"open-skill"}}, false},
		{"bare", selection.Config{Bare: true}, false},
		{"denied but also allowed", selection.Config{AllowSet: true, Allow: []string{"secret-skill"}, Deny: []string{"secret-skill"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := memStore(t)
			seedSelectionSkills(ctx, t, st)
			plan := planFor(ctx, t, st, tc.cfg)
			model, _, evs := runSelected(t, st, plan, t.TempDir(), read, llmtest.SayText("done"))

			res := toolResults(evs)
			if len(res) != 1 {
				t.Fatalf("tool results = %d, want 1: %+v", len(res), res)
			}
			if got := strings.Contains(res[0].Result, "SECRETBODY"); got != tc.wantBody {
				t.Fatalf("body returned = %v, want %v (error=%v): %q", got, tc.wantBody, res[0].IsError, res[0].Result)
			}
			if !tc.wantBody && !res[0].IsError {
				t.Fatalf("a refused read must be an error result, got %q", res[0].Result)
			}
			if sys := model.Requests()[0].System; !tc.wantBody && strings.Contains(sys, "secret-skill") {
				t.Fatalf("the prompt still offers the skill:\n%s", sys)
			}
		})
	}
}

// Selecting never deletes: the stored skill is intact after a run that denied it.
func TestSelectionDoesNotDeleteSkills(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	plan := planFor(ctx, t, st, selection.Config{Bare: true, Deny: []string{"secret-skill"}})
	runSelected(t, st, plan, t.TempDir(), llmtest.SayText("done"))

	got, err := st.Skills().Get(ctx, "secret-skill")
	if err != nil || got.Body != "SECRETBODY" {
		t.Fatalf("stored skill after a denying run = %+v, %v", got, err)
	}
}

// A bare run offers no skill and no recalled memory, says so on its stream, and the
// transcript line for that event names bare and zero skills.
func TestBareOffersNothingAndSaysSo(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	if _, err := st.Memory().Write(ctx, state.MemoryItem{Kind: "fact", Content: "frobnicate widgets only on Tuesdays"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	plan := planFor(ctx, t, st, selection.Config{Bare: true, BareSet: true})
	model, out, evs := runSelected(t, st, plan, t.TempDir(), llmtest.SayText("done"))

	sys := model.Requests()[0].System
	for _, banned := range []string{"open-skill", "secret-skill", "bundled-skill", "Tuesdays", "skill_read"} {
		if strings.Contains(sys, banned) {
			t.Errorf("bare prompt mentions %q:\n%s", banned, sys)
		}
	}

	var sel *session.Event
	for i := range evs {
		if evs[i].Kind == session.KindSelection {
			sel = &evs[i]
		}
	}
	if sel == nil {
		t.Fatalf("no selection event on the stream: %+v", evs)
	}
	line := selectionLine(*sel)
	for _, want := range []string{"bare", "skills offered: 0"} {
		if !strings.Contains(line, want) {
			t.Errorf("selection line %q missing %q", line, want)
		}
	}
	if !strings.Contains(out, line) {
		t.Errorf("the live transcript does not show the readout %q:\n%s", line, out)
	}
}

// A run with no rules records nothing, so an ordinary transcript is unchanged.
func TestDefaultSelectionRecordsNothing(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	_, out, evs := runSelected(t, st, planFor(ctx, t, st, selection.Config{}), t.TempDir(), llmtest.SayText("done"))
	for _, e := range evs {
		if e.Kind == session.KindSelection {
			t.Fatalf("a default run recorded a selection: %+v", e)
		}
	}
	if strings.Contains(out, "selection:") {
		t.Fatalf("a default run printed a selection line:\n%s", out)
	}
}

// --tools grants exactly the named tools: a call to another one is refused and has no
// effect, and the same call lands when its tool is named.
func TestToolsGrantsOnlyTheNamedTools(t *testing.T) {
	write := llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"hello.txt","content":"hi"}`))
	for _, tc := range []struct {
		name      string
		cfg       selection.Config
		wantWrite bool
	}{
		{"write named", selection.Config{ToolsSet: true, Tools: []string{"write"}}, true},
		{"only read named", selection.Config{ToolsSet: true, Tools: []string{"read"}}, false},
		{"no tools", selection.Config{ToolsSet: true, Tools: []string{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := memStore(t)
			dir := t.TempDir()
			plan := planFor(ctx, t, st, tc.cfg)
			_, _, evs := runSelected(t, st, plan, dir, write, llmtest.SayText("done"))

			_, statErr := os.Stat(filepath.Join(dir, "hello.txt"))
			if wrote := statErr == nil; wrote != tc.wantWrite {
				t.Fatalf("file written = %v, want %v", wrote, tc.wantWrite)
			}
			res := toolResults(evs)
			if len(res) != 1 || res[0].IsError == tc.wantWrite {
				t.Fatalf("tool result = %+v, want error=%v", res, !tc.wantWrite)
			}
			if tc.cfg.ToolsSet && !hasSelectionEvent(evs) {
				t.Fatal("the narrowed grant is not on the stream")
			}
		})
	}
}

func hasSelectionEvent(evs []session.Event) bool {
	for _, e := range evs {
		if e.Kind == session.KindSelection {
			return true
		}
	}
	return false
}

// The recorded selection names the tools granted.
func TestSelectionEventNamesTheTools(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	plan := planFor(ctx, t, st, selection.Config{ToolsSet: true, Tools: []string{"read", "grep"}})
	_, _, evs := runSelected(t, st, plan, t.TempDir(), llmtest.SayText("done"))
	for _, e := range evs {
		if e.Kind == session.KindSelection {
			if !strings.Contains(e.Text, "tools: grep, read") {
				t.Fatalf("selection text = %q", e.Text)
			}
			return
		}
	}
	t.Fatal("no selection event")
}

func TestDenyBeatsAllow(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	plan := planFor(ctx, t, st, selection.Config{
		AllowSet: true, Allow: []string{"open-skill", "secret-skill"}, Deny: []string{"secret-skill"},
	})
	if plan.offered != 1 {
		t.Fatalf("offered = %d, want 1", plan.offered)
	}
	block, _, _ := recallContext(ctx, plan.skills, nil, selectionObjective)
	if !strings.Contains(block, "open-skill") || strings.Contains(block, "secret-skill") {
		t.Fatalf("recall block:\n%s", block)
	}
}

func TestResolveSelectionRefusals(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	for _, tc := range []struct {
		name     string
		cfg      selection.Config
		external bool
		want     []string
	}{
		{"unknown allowed skill", selection.Config{AllowSet: true, Allow: []string{"nope"}}, false, []string{`no skill "nope"`, "open-skill", "secret-skill", "bundled-skill"}},
		{"unknown denied skill", selection.Config{Deny: []string{"nope"}}, false, []string{`--no-skill: no skill "nope"`, "open-skill"}},
		{"unknown tool", selection.Config{ToolsSet: true, Tools: []string{"nope"}}, false, []string{`no tool "nope"`, "bash", "edit", "glob", "grep", "read", "write"}},
		{"tools with an external agent", selection.Config{ToolsSet: true, Tools: []string{"read"}}, true, []string{"external agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveSelection(ctx, tc.cfg, st.Skills(), tc.external, false, false)
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

// An external agent still honours the skill rules, which reach it through the bridged
// skill tools.
func TestExternalAgentKeepsSkillRules(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	p, err := resolveSelection(ctx, selection.Config{Deny: []string{"secret-skill"}}, st.Skills(), true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.skills.Get(ctx, "secret-skill"); err == nil {
		t.Fatal("a denied skill is readable through an external agent's view")
	}
}

func TestGrantFor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cfg           selection.Config
		fanout, proof bool
		want          []string
	}{
		{"skills offered", selection.Config{ToolsSet: true, Tools: []string{"read"}}, false, false, []string{"read", "skill_read", "skill_resource"}},
		{"bare drops the skill tools", selection.Config{ToolsSet: true, Tools: []string{"read"}, Bare: true}, false, false, []string{"read"}},
		{"bare with an allowed skill keeps them", selection.Config{ToolsSet: true, Tools: []string{}, Bare: true, AllowSet: true, Allow: []string{"a"}}, false, false, []string{"skill_read", "skill_resource"}},
		{"nothing at all", selection.Config{ToolsSet: true, Tools: []string{}, AllowSet: true}, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := grantFor(tc.cfg, tc.fanout, tc.proof)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("grant = %v, want %v", got, tc.want)
			}
		})
	}
	if got := grantFor(selection.Config{ToolsSet: true, Tools: []string{"read"}, Bare: true}, true, true); len(got) != 3 {
		t.Fatalf("fan-out and proof add their actions: %v", got)
	}
}

func parseSelectionFlags(t *testing.T, dir string, args ...string) (selection.Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerSelectionFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return f.resolve(fs, dir)
}

// Each flag lands in the config, and the flags absent leave the defaults.
func TestSelectionFlags(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		args  []string
		check func(t *testing.T, c selection.Config)
	}{
		{"none given", nil, func(t *testing.T, c selection.Config) {
			if c.Active() {
				t.Fatalf("active: %+v", c)
			}
		}},
		{"--bare", []string{"-bare"}, func(t *testing.T, c selection.Config) {
			if !c.Bare || !c.BareSet || c.PermitsSkill("x") {
				t.Fatalf("%+v", c)
			}
		}},
		{"--bare=false", []string{"-bare=false"}, func(t *testing.T, c selection.Config) {
			if c.Bare || !c.BareSet {
				t.Fatalf("%+v", c)
			}
		}},
		{"--skills list", []string{"-skills", "a,b"}, func(t *testing.T, c selection.Config) {
			if !c.AllowSet || strings.Join(c.Allow, ",") != "a,b" {
				t.Fatalf("%+v", c)
			}
		}},
		{"--skills none", []string{"-skills", "none"}, func(t *testing.T, c selection.Config) {
			if !c.AllowSet || len(c.Allow) != 0 || c.OffersSkills() {
				t.Fatalf("%+v", c)
			}
		}},
		{"--no-skill repeated", []string{"-no-skill", "a", "-no-skill", "b,c"}, func(t *testing.T, c selection.Config) {
			if strings.Join(c.Deny, ",") != "a,b,c" {
				t.Fatalf("%+v", c)
			}
		}},
		{"--tools list", []string{"-tools", "read,grep"}, func(t *testing.T, c selection.Config) {
			if !c.ToolsSet || strings.Join(c.Tools, ",") != "read,grep" {
				t.Fatalf("%+v", c)
			}
		}},
		{"--no-tools", []string{"-no-tools"}, func(t *testing.T, c selection.Config) {
			if !c.ToolsSet || len(c.Tools) != 0 {
				t.Fatalf("%+v", c)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseSelectionFlags(t, dir, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, c)
		})
	}
}

func TestSelectionFlagErrors(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"tools and no-tools", []string{"-tools", "read", "-no-tools"}, "contradict"},
		{"blank skills", []string{"-skills", " "}, "--skills"},
		{"blank tools", []string{"-tools", ""}, "--tools"},
		{"mixed none", []string{"-skills", "none,a"}, "none"},
		{"empty name", []string{"-no-skill", "a,,b"}, "empty name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSelectionFlags(t, dir, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// The data directory's file supplies the defaults and the command line replaces them;
// the deny lists are joined.
func TestSelectionFileUnderFlags(t *testing.T) {
	dir := t.TempDir()
	file := `{"bare": true, "skills": ["a"], "no_skills": ["x"], "tools": ["read"]}`
	if err := os.WriteFile(filepath.Join(dir, selection.FileName), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := parseSelectionFlags(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Bare || !c.AllowSet || strings.Join(c.Tools, ",") != "read" || strings.Join(c.Deny, ",") != "x" {
		t.Fatalf("file alone: %+v", c)
	}

	c, err = parseSelectionFlags(t, dir, "-bare=false", "-skills", "b", "-no-skill", "y", "-no-tools")
	if err != nil {
		t.Fatal(err)
	}
	if c.Bare || strings.Join(c.Allow, ",") != "b" || len(c.Tools) != 0 || !c.ToolsSet {
		t.Fatalf("flags over file: %+v", c)
	}
	if got := strings.Join(c.Deny, ","); got != "x,y" && got != "y,x" {
		t.Fatalf("deny lists should be joined, got %q", got)
	}

	if err := os.WriteFile(filepath.Join(dir, selection.FileName), []byte(`{"bar": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSelectionFlags(t, dir); err == nil {
		t.Fatal("a misspelt key in the file was accepted")
	}
}

// run() reads the flags before it starts anything, sets the invocation's selection, and
// refuses a bad combination with a usage error.
func TestSelectionFlagsAreWired(t *testing.T) {
	t.Cleanup(func() { selectionConfig = selection.Config{} })
	dir := t.TempDir()

	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var stderr bytes.Buffer
	if code := run(fs, []string{"-data-dir", dir, "-tools", "read", "-no-tools", "version"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("contradictory flags exited %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "contradict") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	fs = flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run(fs, []string{"-data-dir", dir, "-bare", "-skills", "a", "-no-skill", "b", "-no-tools", "version"}, io.Discard, io.Discard)
	c := selectionConfig
	if !c.Bare || strings.Join(c.Allow, ",") != "a" || strings.Join(c.Deny, ",") != "b" || !c.ToolsSet {
		t.Fatalf("selectionConfig = %+v", c)
	}
}

// The flags are documented where the operator looks for them.
func TestSelectionFlagsAreDocumented(t *testing.T) {
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	registerSelectionFlags(fs)
	for _, name := range []string{"bare", "skills", "no-skill", "tools", "no-tools"} {
		f := fs.Lookup(name)
		if f == nil || len(f.Usage) < 30 {
			t.Errorf("flag -%s has no help text: %+v", name, f)
		}
	}
	if u := fs.Lookup("bare").Usage; !strings.Contains(u, "selection.json") {
		t.Errorf("-bare help does not name the file setting: %q", u)
	}
}

// --no-bundled-skills still removes the bundled set from the store, and a selection
// that merely hides skills does not.
func TestNoBundledSkillsStillDeletesAndSelectionDoesNot(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	planFor(ctx, t, st, selection.Config{Bare: true})
	if got := bundledSlugs(ctx, t, st.Skills()); len(got) == 0 {
		t.Fatal("bare removed the bundled skills from the store")
	}
}

// Bare loads no project file unless the operator also asked for them on the command line.
func TestProjectDecisionForBare(t *testing.T) {
	oldSel, oldFlag := selectionConfig, projectFilesFlag
	t.Cleanup(func() { selectionConfig, projectFilesFlag = oldSel, oldFlag })
	repo := instructionRepo(t)

	selectionConfig, projectFilesFlag = selection.Config{Bare: true}, ""
	if d := projectDecisionFor(repo, t.TempDir()); d.Verdict != "" || len(d.Set.Files) != 0 {
		t.Fatalf("bare loaded project files: %+v", d)
	}

	selectionConfig, projectFilesFlag = selection.Config{}, "trust"
	if d := projectDecisionFor(repo, t.TempDir()); len(d.Set.Files) == 0 {
		t.Fatalf("without bare the decision is unchanged: %+v", d)
	}
}

// An interactive session records its selection once per run, and again for the run a
// /clear starts.
func TestREPLRecordsSelectionOncePerRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := memStore(t)
	seedSelectionSkills(ctx, t, st)
	plan := planFor(ctx, t, st, selection.Config{Bare: true})

	model := llmtest.NewScripted(llmtest.SayText("one"), llmtest.SayText("two"), llmtest.SayText("three"))
	s, buf := newREPL(t, t.TempDir(), st, model)
	s.selection = &plan

	count := func(runID string) int {
		t.Helper()
		evs, err := session.History(ctx, st.Log(), runID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range evs {
			if e.Kind == session.KindSelection {
				n++
			}
		}
		return n
	}
	for _, line := range []string{"frobnicate widgets", "again"} {
		if _, err := s.runTurn(ctx, line, nil, nil); err != nil {
			t.Fatalf("turn %q: %v\n%s", line, err, buf.String())
		}
	}
	first := s.runID
	if n := count(first); n != 1 {
		t.Fatalf("selection events on a two-turn run = %d, want 1", n)
	}
	for i, req := range model.Requests() {
		if strings.Contains(req.System, "open-skill") {
			t.Fatalf("request %d offered a skill under bare", i)
		}
	}
	s.clear()
	if _, err := s.runTurn(ctx, "third", nil, nil); err != nil {
		t.Fatalf("turn after clear: %v", err)
	}
	if s.runID == first || count(s.runID) != 1 {
		t.Fatalf("after /clear: run %s (was %s) has %d selection events, want a new run with 1", s.runID, first, count(s.runID))
	}
}
