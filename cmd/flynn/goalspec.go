package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/goal"
)

// A goal spec file holds a run's terms (what must stay true while the work happens, and
// how each is observed) alongside its objective, stop condition and allowances.
//
// It is a file because a term's check is a shell command, and a flag like
// `--invariant "clean:git diff --quiet -- . ':!vendor'"` fights the operator's shell on
// the first quote or colon.
//
// It is JSON, like the shipped playbook specs, and decodes into a curated struct in
// place of goal.Spec. The fields the run owns (model, step budget, grant, the planner's
// unit graph) already have a flag or an owner, and hand-editing them is unsafe, so the
// file carries only the operator's half.

// goalSpecFile is the operator's half of a goal spec.
type goalSpecFile struct {
	// Objective may be given here instead of on the command line, so a spec file is a
	// complete, re-runnable statement of a run.
	Objective string `json:"objective,omitempty"`
	// StopCondition is what must become true for the work to be over. Omitted, the run uses
	// the default of a bare `flynn goal`.
	StopCondition string `json:"stopCondition,omitempty"`

	Invariants []goal.Invariant `json:"invariants,omitempty"`
	// Allowances are the irreversible actions outside the workspace this run may take. Only
	// here can one be narrowed to a target; --allow cannot express that.
	Allowances []goal.Allowance `json:"allowances,omitempty"`
	// Skills are loaded into the run's first turn in this order, after any named with
	// --skill.
	Skills []string `json:"skills,omitempty"`
}

// errEmptyGoalSpec refuses a spec that parsed but states nothing, since running would
// leave the operator believing a file was in force.
var errEmptyGoalSpec = errors.New("states nothing: it needs at least an objective or one invariant")

// loadGoalSpecFile reads and validates a goal spec file; "-" reads stdin.
//
// Unknown fields are refused. A misspelled `"invariant"` would otherwise load, run and
// record a goal held to nothing, indistinguishable from one whose terms held.
func loadGoalSpecFile(path string) (goalSpecFile, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		// #nosec G304 -- the path is the one the operator typed after --goal-spec, on
		// their own machine, and reading it is the whole point of the flag. There is no
		// confinement to add: a person naming a file for a command they are running is
		// not a file-inclusion risk.
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return goalSpecFile{}, fmt.Errorf("goal spec %s: %w", path, err)
	}

	var spec goalSpecFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return goalSpecFile{}, fmt.Errorf("goal spec %s: %w\n%s", path, err, goalSpecExample)
	}
	spec.Objective = strings.TrimSpace(spec.Objective)
	spec.StopCondition = strings.TrimSpace(spec.StopCondition)
	// Terms are trimmed before adoption. An invariant is fingerprinted when adopted, so
	// mid-run rewording is detectable, and a stray heredoc newline must not join the term.
	for i := range spec.Invariants {
		spec.Invariants[i].ID = strings.TrimSpace(spec.Invariants[i].ID)
		spec.Invariants[i].Statement = strings.TrimSpace(spec.Invariants[i].Statement)
		spec.Invariants[i].Check = strings.TrimSpace(spec.Invariants[i].Check)
	}
	if err := spec.validate(); err != nil {
		return goalSpecFile{}, fmt.Errorf("goal spec %s: %w", path, err)
	}
	return spec, nil
}

// validate refuses, while the operator is reading their own file, a spec the engine
// would refuse later. [goal.ValidateInvariants] decides admission; this adds only what
// the file can get wrong on its own.
func (s goalSpecFile) validate() error {
	if s.Objective == "" && len(s.Invariants) == 0 {
		return errEmptyGoalSpec
	}
	if err := goal.ValidateInvariants(s.Invariants); err != nil {
		// The engine's error names the rule; naming the offending term makes the file editable.
		// An id-less term is named by position.
		return fmt.Errorf("%w%s", err, offendingTerms(s.Invariants))
	}
	for i, a := range s.Allowances {
		if strings.TrimSpace(a.Action) == "" {
			return fmt.Errorf("allowance %d declares no action: an allowance authorizes a named dispatch action, e.g. {\"action\": \"shell\"}", i+1)
		}
	}
	return nil
}

// offendingTerms names the terms a validation failure concerns: duplicates and absence
// claims by id, an incomplete term by position. It returns "" when it cannot narrow it
// down.
func offendingTerms(invs []goal.Invariant) string {
	var named []string
	seen := map[string]int{}
	for i, inv := range invs {
		id, statement := strings.TrimSpace(inv.ID), strings.TrimSpace(inv.Statement)
		switch {
		case id == "" || statement == "":
			named = append(named, fmt.Sprintf("invariant %d needs both an id and a statement", i+1))
		case seen[id] > 0:
			named = append(named, fmt.Sprintf("%q is declared twice (invariants %d and %d)", id, seen[id], i+1))
		case inv.Check == "" && goal.AssertsAbsence(statement):
			named = append(named, fmt.Sprintf("%q claims something is not there, so it needs a \"check\": the command that would find a counterexample, e.g. \"! grep -r SECRET .\"", id))
		}
		if id != "" && seen[id] == 0 {
			seen[id] = i + 1
		}
	}
	if len(named) == 0 {
		return ""
	}
	return "\n  " + strings.Join(named, "\n  ")
}

// goalSpecExample is printed when a file will not parse, so the operator sees the shape.
const goalSpecExample = `a goal spec is JSON:
  {
    "objective": "upgrade the http client and keep the suite green",
    "invariants": [
      {
        "id": "public-api",
        "statement": "the exported API of ./client does not change",
        "check": "./dev/apidiff ./client"
      }
    ],
    "allowances": [{"action": "shell"}]
  }`

// mergeGoalSpec folds the spec file and the command line into the run's objective and
// stop condition.
//
// An objective in both places is an error. It means a stale file or a forgotten command
// line, and any precedence rule would silently run the other one. The message names the
// two to reconcile.
func mergeGoalSpec(spec goalSpecFile, objective string) (string, error) {
	objective = strings.TrimSpace(objective)
	switch {
	case objective != "" && spec.Objective != "" && objective != spec.Objective:
		return "", fmt.Errorf("the objective is stated twice and the two differ:\n  on the command line: %q\n  in the goal spec:    %q\nremove one of them", objective, spec.Objective)
	case objective != "":
		return objective, nil
	case spec.Objective != "":
		return spec.Objective, nil
	}
	return "", errors.New(`usage: flynn goal "<objective>", or state "objective" in a --goal-spec file`)
}

// mergeAllowances concatenates the --allow declarations and the spec file's, since each
// is a separate authorization the operator wrote. Duplicates collapse.
func mergeAllowances(spec goalSpecFile, allowed []string) []goal.Allowance {
	out := append([]goal.Allowance(nil), declaredAllowances(allowed)...)
	seen := map[goal.Allowance]bool{}
	for _, a := range out {
		seen[a] = true
	}
	extra := make([]goal.Allowance, 0, len(spec.Allowances))
	for _, a := range spec.Allowances {
		a = goal.Allowance{Action: strings.TrimSpace(a.Action), Target: strings.TrimSpace(a.Target)}
		if !seen[a] {
			seen[a] = true
			extra = append(extra, a)
		}
	}
	sort.Slice(extra, func(i, j int) bool {
		if extra[i].Action != extra[j].Action {
			return extra[i].Action < extra[j].Action
		}
		return extra[i].Target < extra[j].Target
	})
	return append(out, extra...)
}

// mergeSkills lists the skills named for the run: the --skill flags in the order given,
// then the spec file's, each once. Names are trimmed and blanks dropped.
func mergeSkills(flags, file []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range append(append([]string(nil), flags...), file...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// termsLines is what a run says about its terms before it starts, one line per term.
//
// A term without a check is judged by the model auditor from the run's own record; one
// with a check is judged by running it. The file leaves the check optional, so the run
// states the difference while the operator can still add one.
func termsLines(invs []goal.Invariant) []string {
	if len(invs) == 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("  terms (%d): a breach stops the run", len(invs))}
	for _, inv := range invs {
		how := "judged by the auditor model from the run's record"
		if inv.Check != "" {
			how = "checked by: " + inv.Check
		}
		lines = append(lines, fmt.Sprintf("    %s: %s (%s)", inv.ID, inv.Statement, how))
	}
	return lines
}
