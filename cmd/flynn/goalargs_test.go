package main

import (
	"flag"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// goalFlagSet is a flag set with the shape of the binary's: a string flag, a boolean,
// and a repeatable one, which are the three ways a run flag is written.
func goalFlagSet() (*flag.FlagSet, *string, *bool, *stringList) {
	fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	spec := fs.String("goal-spec", "", "")
	fanout := fs.Bool("fanout", false, "")
	approve := &stringList{}
	fs.Var(approve, "require-approval", "")
	return fs, spec, fanout, approve
}

func TestGoalArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantRest   []string
		wantSpec   string
		wantFanout bool
		wantApprov []string
		wantUsage  string
	}{
		{
			name:     "flags after goal are read, not folded into the objective",
			args:     []string{"goal", "--goal-spec", "terms.json", "--require-approval", "shell", "deploy", "it"},
			wantRest: []string{"goal", "deploy", "it"}, wantSpec: "terms.json", wantApprov: []string{"shell"},
		},
		{
			name:     "a boolean flag after goal",
			args:     []string{"goal", "--fanout", "split the work"},
			wantRest: []string{"goal", "split the work"}, wantFanout: true,
		},
		{
			name:     "flags before goal still work and leave the objective alone",
			args:     []string{"--fanout", "goal", "split the work"},
			wantRest: []string{"goal", "split the work"}, wantFanout: true,
		},
		{
			name:      "a flag after the objective is refused, naming it",
			args:      []string{"goal", "deploy it", "--require-approval", "shell"},
			wantUsage: "--require-approval is a flag",
		},
		{
			name:      "the key=value spelling after the objective is refused too",
			args:      []string{"goal", "deploy it", "--goal-spec=terms.json"},
			wantUsage: "--goal-spec=terms.json is a flag",
		},
		{
			name:     "a dash word that names no flag is objective text",
			args:     []string{"goal", "explain", "-x"},
			wantRest: []string{"goal", "explain", "-x"},
		},
		{
			name:     "after --, a flag name is objective text",
			args:     []string{"goal", "--", "--fanout", "is", "broken"},
			wantRest: []string{"goal", "--fanout", "is", "broken"},
		},
		{
			name:     "a sentence that mentions a flag is objective text",
			args:     []string{"goal", "document what --fanout does"},
			wantRest: []string{"goal", "document what --fanout does"},
		},
		{
			name:     "another command is left as it came",
			args:     []string{"runs", "--fanout"},
			wantRest: []string{"runs", "--fanout"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, spec, fanout, approve := goalFlagSet()
			if err := fs.Parse(c.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			rest, usage := goalArgs(fs, fs.Args())
			if c.wantUsage != "" {
				if !strings.Contains(usage, c.wantUsage) {
					t.Fatalf("usage = %q, want it to contain %q", usage, c.wantUsage)
				}
				return
			}
			if usage != "" {
				t.Fatalf("unexpected usage error: %s", usage)
			}
			if !slices.Equal(rest, c.wantRest) {
				t.Errorf("rest = %q, want %q", rest, c.wantRest)
			}
			if *spec != c.wantSpec || *fanout != c.wantFanout || !slices.Equal(approve.values, c.wantApprov) {
				t.Errorf("flags = spec %q fanout %v approval %q, want %q %v %q",
					*spec, *fanout, approve.values, c.wantSpec, c.wantFanout, c.wantApprov)
			}
		})
	}
}

// TestGoalReadsAGoalSpecWrittenAfterIt is the usage text's own example run through the
// binary's dispatch: the spec named after `goal` is loaded, which a missing file proves
// by failing to load as a usage error before any model is resolved. Before, the flag was
// folded into the objective and the run went ahead without its terms.
func TestGoalReadsAGoalSpecWrittenAfterIt(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "terms.json")
	got := runCLIIn(t, t.TempDir(), "goal", "--goal-spec", missing)
	if got.code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr %q)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "terms.json") {
		t.Fatalf("stderr = %q, want the spec it tried to load named", got.stderr)
	}
}

// TestGoalRefusesAFlagAfterTheObjective: a governing flag typed after the objective is a
// usage error, not words of the objective and an ungoverned run.
func TestGoalRefusesAFlagAfterTheObjective(t *testing.T) {
	got := runCLIIn(t, t.TempDir(), "goal", "deploy the site", "--require-approval", "shell")
	if got.code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr %q)", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "--require-approval is a flag") {
		t.Fatalf("stderr = %q, want the stray flag named", got.stderr)
	}
}
