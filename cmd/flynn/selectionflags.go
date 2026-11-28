package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/ionalpha/flynn/evidence"
	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/internal/selection"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
	"github.com/ionalpha/flynn/tools"
)

// selectionConfig is the invocation's skill and tool selection: the data directory's
// selection file with the command line laid over it. It is a package variable for the
// same reason as bundledSkillsDisabled: every entry point that starts a run reads it.
var selectionConfig selection.Config

// selectionFlags are the flags that choose what a run is offered.
type selectionFlags struct {
	bare    *bool
	skills  *string
	noSkill *stringList
	tools   *string
	noTools *bool
}

func registerSelectionFlags(fs *flag.FlagSet) selectionFlags {
	f := selectionFlags{noSkill: &stringList{}}
	f.bare = fs.Bool("bare", false, "run a plain core loop: no skills, project files or recalled memory")
	f.skills = fs.String("skills", "", "offer only these skills (`slugs`, comma-separated, or \"none\")")
	fs.Var(f.noSkill, "no-skill", "never offer or load these skills (`slugs`; repeatable)")
	f.tools = fs.String("tools", "", "grant only these core tools (`names`, comma-separated, or \"none\")")
	f.noTools = fs.Bool("no-tools", false, "grant no core tools, the same as --tools none")
	return f
}

// selectionCommands are the commands that apply the selection to the runs they start. The
// router refuses a selection flag on any other, because ignoring it would let an operator
// believe a skill was withheld from a run that could still load it.
var selectionCommands = map[string]bool{"": true, "goal": true, "resume": true, "watch": true}

// named is the selection flags the command line set, spelled as the operator typed them.
func (f selectionFlags) named(fs *flag.FlagSet) []string {
	var names []string
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "bare", "skills", "no-skill", "tools", "no-tools":
			names = append(names, "--"+fl.Name)
		}
	})
	return names
}

// resolve turns the parsed flags and the data directory's selection file into one
// [selection.Config]. A flag the command line states replaces the file's; the deny lists
// are joined.
func (f selectionFlags) resolve(fs *flag.FlagSet, dataDir string) (selection.Config, error) {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })

	var c selection.Config
	if set["bare"] {
		c.Bare, c.BareSet = *f.bare, true
	}
	if set["skills"] {
		l, err := selection.ParseList("--skills", *f.skills)
		if err != nil {
			return c, err
		}
		c.Allow, c.AllowSet = l, true
	}
	for _, v := range f.noSkill.values {
		l, err := selection.ParseList("--no-skill", v)
		if err != nil {
			return c, err
		}
		c.Deny = append(c.Deny, l...)
	}
	switch {
	case set["tools"] && *f.noTools:
		return c, errors.New("--tools and --no-tools contradict each other; give one")
	case set["tools"]:
		l, err := selection.ParseList("--tools", *f.tools)
		if err != nil {
			return c, err
		}
		c.Tools, c.ToolsSet = l, true
	case *f.noTools:
		c.Tools, c.ToolsSet = []string{}, true
	}
	file, err := selection.Load(dataDir)
	if err != nil {
		return c, err
	}
	return c.Over(file), nil
}

// coreToolNames are the tools a native run offers and --tools may name.
func coreToolNames() []string {
	var names []string
	for _, t := range tools.New(nil).Tools() {
		names = append(names, t.Def().Name)
	}
	return names
}

// selectionPlan is a selection checked against the store it will run over: the view of the
// skills the run is offered from, how many that is, and the capability grant when the
// tools are narrowed.
type selectionPlan struct {
	cfg selection.Config
	// skills is the store as the run sees it. Learning keeps writing to, and reinforcing
	// through, the unfiltered store.
	skills  state.SkillStore
	offered int
	// grant is the names handed to governed.WithCapabilities; nil leaves the grant alone.
	grant []string
}

// resolveSelection checks cfg against the skills stored and the tools that exist, and
// builds the plan. Every refusal names what would have been valid, so a misspelling is
// caught at startup rather than as a skill that is silently missing.
func resolveSelection(ctx context.Context, cfg selection.Config, store state.SkillStore, external, fanout, proof bool) (selectionPlan, error) {
	if external && cfg.ToolsSet {
		return selectionPlan{}, errors.New("--tools and --no-tools do not apply to an external agent backend: it runs its own tools, and the grant cannot bound them")
	}
	var known []string
	permitted := 0
	for _, src := range skillSources {
		list, err := store.List(ctx, src.scope)
		if err != nil {
			return selectionPlan{}, err
		}
		for _, s := range list {
			if !slices.Contains(known, s.Slug) {
				known = append(known, s.Slug)
				if cfg.PermitsSkill(s.Slug) {
					permitted++
				}
			}
		}
	}
	if err := cfg.CheckSkills(known); err != nil {
		return selectionPlan{}, err
	}
	if err := cfg.CheckTools(coreToolNames()); err != nil {
		return selectionPlan{}, err
	}
	p := selectionPlan{cfg: cfg, skills: selection.Skills(store, cfg), offered: permitted}
	if cfg.ToolsSet {
		p.grant = grantFor(cfg, fanout, proof)
	}
	return p, nil
}

// grantFor is the capability list for a narrowed tool set. The tools the operator named
// are not the whole grant: the skill tools ride with the skill selection, a fan-out needs
// its spawn action, and a proof run needs the verification action. Each is added only when
// the run uses it, because a grant is also what a run is refused outside of.
func grantFor(cfg selection.Config, fanout, proof bool) []string {
	grant := slices.Clone(cfg.Tools)
	if cfg.OffersSkills() {
		grant = append(grant, "skill_read", "skill_resource")
	}
	if fanout {
		grant = append(grant, mission.ActionSpawn)
	}
	if proof {
		grant = append(grant, evidence.VerifyItemAction)
	}
	return grant
}

// recallFrom is the skill store a run offers from and the memory store it recalls and
// digests from. A bare plan returns no memory store, which is what tells the caller to
// leave the digest out; a nil plan is the default selection and changes neither.
func (p *selectionPlan) recallFrom(skills state.SkillStore, memories state.MemoryStore) (state.SkillStore, state.MemoryStore) {
	if p == nil {
		return skills, memories
	}
	if p.cfg.Bare {
		return p.skills, nil
	}
	return p.skills, memories
}

// withSelection hands a resolved selection to a run.
func withSelection(p selectionPlan) driveOption {
	return func(c *driveConfig) { c.selection = &p }
}

// toolSummary is the core tools the run is granted, for the record and the readout.
func (p selectionPlan) toolSummary() []string {
	if !p.cfg.ToolsSet {
		return nil
	}
	return p.cfg.Tools
}

// projectDecisionFor is the project file decision for a run under the invocation's
// selection. Bare asks for a plain loop, so it loads none unless --project-files was given
// on the command line, which is the more specific statement.
func projectDecisionFor(workdir, dataDir string) projectctx.Decision {
	if selectionConfig.Bare && projectFilesFlag == "" {
		return projectctx.Decision{}
	}
	return projectDecision(workdir, dataDir)
}

// recordSelection writes the selection onto the run's stream, so a later `flynn inspect`
// shows what the run was offered. A run with the default selection records nothing. A
// failure is reported and never fails the run.
func recordSelection(ctx context.Context, out io.Writer, log spine.Log, p *selectionPlan, runID string) {
	if p == nil || !p.cfg.Active() {
		return
	}
	if err := selection.Append(ctx, log, runID, p.cfg, selection.Summary{Offered: p.offered, Tools: p.toolSummary()}); err != nil {
		_, _ = fmt.Fprintf(out, "  (selection not recorded: %v)\n", err)
	}
}

// skillView is the skills the session offers and lets skill_read load.
func (s *replSession) skillView() state.SkillStore {
	if s.selection == nil || s.selection.skills == nil {
		return s.store.Skills()
	}
	return s.selection.skills
}
