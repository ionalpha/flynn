package skilltool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/state"
)

// Property: whatever a skill's body is, skill_read either returns the whole of it or
// returns nothing and an error. It never returns part.
//
// This is the contract the task exists to establish, and a partial return is exactly
// what a length guard produces when someone later decides a refusal is unfriendly.
// A property states it once for every body rather than for the three lengths a person
// would think to write.
func TestProp_AReadIsWholeOrNothing(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		body := strings.Repeat(rapid.StringMatching(`[a-z .]{1,40}`).Draw(rt, "chunk"),
			rapid.IntRange(1, 40).Draw(rt, "repeats"))

		tools := over(rt, state.Skill{
			Slug: "tidy-diff", Name: "tidy-diff",
			Description: "Reduce a change to the smallest diff that still does the job.",
			Body:        body, Scope: state.BundledScope,
		})

		got, err := invoke(rt, tools["skill_read"], map[string]string{"skill": "tidy-diff"})
		trimmed := strings.TrimSpace(body)
		switch {
		case err != nil:
			if got != "" {
				rt.Errorf("a refusal returned %d characters as well as an error", len(got))
			}
		case !strings.Contains(got, trimmed):
			rt.Errorf("read returned %d characters of a %d-character body", len(got), len(trimmed))
		}
	})
}

// Property: skill_resource answers for a path the skill addresses and refuses every
// other string, whatever that string is.
//
// The generator draws paths that look like the real ones, including the near misses
// a traversal attempt produces, because the failure worth catching is a check that
// reasons about what a path means rather than asking whether the loader listed it.
func TestProp_OnlyAddressedPathsAreReadable(t *testing.T) {
	addressed := []string{"references/checklist.md", "scripts/run.sh"}
	rapid.Check(t, func(rt *rapid.T) {
		tools := over(rt, bundledSkill())

		path := rapid.OneOf(
			rapid.SampledFrom(addressed),
			rapid.SampledFrom([]string{
				"SKILL.md", "references", "scripts/", "/etc/passwd", "",
				"../tidy-diff/scripts/run.sh", "./references/checklist.md",
				"references//checklist.md", "REFERENCES/CHECKLIST.MD",
				"references/checklist.md ", "scripts/run.sh/../run.sh",
			}),
			rapid.StringMatching(`[a-z./]{0,12}`),
		).Draw(rt, "path")

		_, err := invoke(rt, tools["skill_resource"], map[string]string{"skill": "tidy-diff", "path": path})
		want := false
		for _, a := range addressed {
			if path == a {
				want = true
			}
		}
		if got := err == nil; got != want {
			rt.Errorf("skill_resource(%q): readable = %v, want %v (err = %v)", path, got, want, err)
		}
	})
}

// over builds the toolset over a store holding sk and the pack the tests share.
func over(rt *rapid.T, sk state.Skill) map[string]mission.Tool {
	skills := state.NewMemory().Skills()
	if _, err := skills.Upsert(context.Background(), sk); err != nil {
		rt.Fatalf("seed skill: %v", err)
	}
	out := map[string]mission.Tool{}
	for _, tool := range skilltool.New(skills, skilltool.WithPack(propPack(), "skills")).Tools() {
		out[tool.Def().Name] = tool
	}
	return out
}

// propPack is the shared tree, kept beside the property tests so the addressed set
// the properties assert against is the one the loader reads.
func propPack() fstest.MapFS {
	return fstest.MapFS{
		"skills/tidy-diff/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: tidy-diff\ndescription: Reduce a change to the smallest diff that still does the job, before asking anyone to read it.\n---\n\nRead the diff as a reviewer would.\n")},
		"skills/tidy-diff/references/checklist.md": &fstest.MapFile{Data: []byte("1. Delete what the change does not need.\n")},
		"skills/tidy-diff/scripts/run.sh":          &fstest.MapFile{Data: []byte("#!/bin/sh\nexit 0\n")},
	}
}

func invoke(rt *rapid.T, tool mission.Tool, args any) (string, error) {
	b, err := json.Marshal(args)
	if err != nil {
		rt.Fatalf("marshal args: %v", err)
	}
	return tool.Invoke(context.Background(), b)
}
