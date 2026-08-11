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

// A goal spec file is where an operator writes the terms of a run: what must stay true
// while the work happens, and how each of those terms is observed.
//
// It is a file rather than a repeatable flag because a term's check is a shell command.
// `--invariant "the tree stays clean:git diff --quiet -- . ':!vendor'"` puts the operator
// in a delimiter fight with their own shell on the first term that has a quote or a colon
// in it, and the terms of a run are the last thing that should be awkward to state
// precisely. A file also holds the objective, the stop condition and the allowances
// together, which is what a run is: the thing to do, when it is over, what may not be
// traded away to get there, and what it is authorized to reach for.
//
// The format is JSON, matching the playbook specs already shipped in this repository, and
// it decodes into a curated struct rather than straight into goal.Spec. The spec carries
// fields the run owns (the model, the step budget, the grant, the unit graph the planner
// writes); a file that could set those would let an operator hand-edit machinery they have
// no way to reason about, and every one of them already has a flag or an owner. What is
// left is exactly the operator's half.

// goalSpecFile is the operator-facing half of a goal spec: what the run is for, when it is
// over, the terms it is held to, and the irreversible actions it may take.
type goalSpecFile struct {
	// Objective is what the run is asked to do. It may be given here instead of on the
	// command line, so a spec file is a complete, re-runnable statement of a run.
	Objective string `json:"objective,omitempty"`
	// StopCondition is what must become true for the work to be over. Omitted, the run
	// uses the same default a bare `flynn goal` does.
	StopCondition string `json:"stopCondition,omitempty"`
	// Invariants are the terms of the run. This is the field the file exists for.
	Invariants []goal.Invariant `json:"invariants,omitempty"`
	// Allowances are the irreversible actions outside the workspace this run is
	// authorized to take. The narrow, target-bound form is expressible here and is not
	// expressible with --allow.
	Allowances []goal.Allowance `json:"allowances,omitempty"`
}

// errEmptyGoalSpec reports a spec file that parsed but states nothing. Loading it and
// running anyway would give the operator a run governed by a file they believe is in
// force, which is the one outcome worse than refusing.
var errEmptyGoalSpec = errors.New("states nothing: it needs at least an objective or one invariant")

// loadGoalSpecFile reads and validates a goal spec file. The path "-" reads stdin, so a
// spec can be generated and piped without touching the disk.
//
// Unknown fields are refused. A misspelled key in a governance file is the failure this
// surface exists to prevent: `"invariant"` for `"invariants"` would otherwise load
// cleanly, run to completion, and produce a record showing a goal that was never held to
// anything, indistinguishable from one whose terms held.
func loadGoalSpecFile(path string) (goalSpecFile, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
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
	// Trim what the file states before anything reads it, so the term the run adopts is
	// the term the operator wrote. An invariant's content is fingerprinted at adoption
	// and the fingerprint is what makes rewording it mid-run detectable, so a stray
	// newline off a heredoc must not become part of what the term says.
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

// validate refuses a spec the engine would refuse later, and refuses it here, where the
// operator is still reading their own file. The rules are the engine's: goal.Validate
// Invariants is the one that decides admission, so it is called rather than restated, and
// this adds only what the file surface can get wrong on its own.
func (s goalSpecFile) validate() error {
	if s.Objective == "" && len(s.Invariants) == 0 {
		return errEmptyGoalSpec
	}
	if err := goal.ValidateInvariants(s.Invariants); err != nil {
		// The engine's error names the rule that was broken; naming the term that broke
		// it is what makes the file editable. An id-less term is pointed at by position,
		// since there is nothing else to call it.
		return fmt.Errorf("%w%s", err, offendingTerms(s.Invariants))
	}
	for i, a := range s.Allowances {
		if strings.TrimSpace(a.Action) == "" {
			return fmt.Errorf("allowance %d declares no action: an allowance authorizes a named dispatch action, e.g. {\"action\": \"shell\"}", i+1)
		}
	}
	return nil
}

// offendingTerms names the terms a validation failure is about, so the operator can find
// them in a file with several. It reports duplicates by id, absence claims by id, and an
// incomplete term by its position, and returns "" when it cannot narrow it down (the
// engine's own message stands alone in that case).
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

// goalSpecExample is printed when a file will not parse. Somebody who has just been told
// their JSON is malformed wants the shape, not a pointer to documentation they have to go
// and find.
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

// mergeGoalSpec folds a loaded spec file and the command line into the one objective and
// stop condition the run is submitted with.
//
// A conflict is an error rather than a precedence rule. Both an objective on the command
// line and an objective in the file is somebody running a spec file they have edited past
// or a command line they have forgotten; silently picking a winner means the run does the
// other one, and "which wins" is a thing nobody remembers under time pressure. The message
// says which two to reconcile.
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

// mergeAllowances folds the --allow declarations and the spec file's into one list. The
// flag's form is action-level and the file's may narrow to a target, so the two are
// concatenated rather than one overriding the other: each is a separate authorization the
// operator wrote down. Duplicates collapse.
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

// termsLines is what a run says about its terms before it starts, one line per term.
//
// It is here because a term with no check is judged by the model auditor, which rules on
// the run's own record, and a term with one is judged by running it. That is a real
// difference in what the operator is getting, and the file makes the check optional
// without ever saying so. The run says it, at the point where the operator can still
// stop and write the check.
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
