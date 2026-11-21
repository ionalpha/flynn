package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/spine"
)

// builtinSpec is the --model flag's default, the suggestion that must never decide
// which provider a run uses.
const builtinSpec = "anthropic:claude-opus-5-5"

// askWith replaces the resolver's streams for one test: whether a person is at a
// terminal, and what they type. It returns what the resolver wrote to them.
func askWith(t *testing.T, terminal bool, input string) *bytes.Buffer {
	t.Helper()
	out := &bytes.Buffer{}
	prior := modelAskIO
	modelAskIO = func() modelAsk {
		return modelAsk{terminal: terminal, in: strings.NewReader(input), out: out}
	}
	t.Cleanup(func() { modelAskIO = prior })
	return out
}

// keyedEnv gives each named provider a key in the environment and no other.
func keyedEnv(t *testing.T, names ...string) {
	t.Helper()
	noProviderKeys(t)
	env := map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY", "gemini": "GEMINI_API_KEY"}
	for _, n := range names {
		t.Setenv(env[n], "test-key")
	}
}

// TestResolveModelChoice drives the resolver through the choices an operator can make
// (a flag, a saved default, nothing) against the providers that have a key and whether
// anyone can be asked.
func TestResolveModelChoice(t *testing.T) {
	cases := []struct {
		name       string
		keys       []string
		saved      string
		origin     modelOrigin
		spec       string
		terminal   bool
		input      string
		wantSpec   string
		wantSource string
		wantSaved  string
		wantErr    error
		errHas     []string
	}{
		{
			name: "flag is used as named even with other keys", keys: []string{"anthropic", "openai"},
			origin: modelFromFlag, spec: "openai:gpt-5.5",
			wantSpec: "openai:gpt-5.5", wantSource: sourceFlag,
		},
		{
			name: "saved default is used as named", keys: []string{"anthropic", "openai"},
			origin: modelFromSaved, spec: "openai:gpt-5.5", saved: "openai:gpt-5.5",
			wantSpec: "openai:gpt-5.5", wantSource: sourceSaved, wantSaved: "openai:gpt-5.5",
		},
		{
			name: "flag without a key is refused, not redirected", keys: []string{"openai"},
			origin: modelFromFlag, spec: builtinSpec,
			wantErr: provider.ErrCredentialNotSet, errHas: []string{"anthropic", "flynn auth set anthropic", "--model", "flynn models use"},
		},
		{
			name: "saved default without a key is refused, not redirected", keys: []string{"openai"},
			origin: modelFromSaved, spec: builtinSpec, saved: builtinSpec,
			wantErr: provider.ErrCredentialNotSet, errHas: []string{"anthropic", "saved default"},
			wantSaved: builtinSpec,
		},
		{
			name: "two keys and no terminal refuse and name both ways out", keys: []string{"anthropic", "openai"},
			origin: modelUnchosen, spec: builtinSpec,
			wantErr: errModelNotChosen, errHas: []string{"anthropic", "openai", "--model", "flynn models use"},
		},
		{
			name: "two keys and a terminal ask, and a number is saved", keys: []string{"anthropic", "openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "2\n",
			wantSpec: "openai:" + defaultModelOf(t, "openai"), wantSource: sourcePrompt + savedAsDefaultTag,
			wantSaved: "openai:" + defaultModelOf(t, "openai"),
		},
		{
			name: "two keys and an empty answer takes the suggestion", keys: []string{"anthropic", "openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "\n",
			wantSpec: builtinSpec, wantSource: sourcePrompt + savedAsDefaultTag, wantSaved: builtinSpec,
		},
		{
			name: "two keys and a typed provider:model is saved", keys: []string{"anthropic", "openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "openai:gpt-5.5\n",
			wantSpec: "openai:gpt-5.5", wantSource: sourcePrompt + savedAsDefaultTag, wantSaved: "openai:gpt-5.5",
		},
		{
			name: "two keys and an answer naming an unkeyed provider is refused", keys: []string{"anthropic", "openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "gemini\n",
			wantErr: errModelNotChosen, errHas: []string{"gemini"},
		},
		{
			name: "two keys and an out-of-range number is refused", keys: []string{"anthropic", "openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "9\n",
			wantErr: errModelNotChosen,
		},
		{
			name: "one key and no terminal uses it and saves nothing", keys: []string{"openai"},
			origin: modelUnchosen, spec: builtinSpec,
			wantSpec: "openai:" + defaultModelOf(t, "openai"), wantSource: sourceOnlyKeyed,
		},
		{
			name: "one key and a terminal yes saves it", keys: []string{"openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "y\n",
			wantSpec: "openai:" + defaultModelOf(t, "openai"), wantSource: sourceOnlyKeyed + savedAsDefaultTag,
			wantSaved: "openai:" + defaultModelOf(t, "openai"),
		},
		{
			name: "one key and a terminal default answer saves it", keys: []string{"openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "\n",
			wantSpec: "openai:" + defaultModelOf(t, "openai"), wantSource: sourceOnlyKeyed + savedAsDefaultTag,
			wantSaved: "openai:" + defaultModelOf(t, "openai"),
		},
		{
			name: "one key and a terminal no refuses and saves nothing", keys: []string{"openai"},
			origin: modelUnchosen, spec: builtinSpec, terminal: true, input: "n\n",
			wantErr: errModelNotChosen, errHas: []string{"--model", "flynn models use"},
		},
		{
			name: "one key matching the built-in provider keeps the built-in model", keys: []string{"anthropic"},
			origin: modelUnchosen, spec: builtinSpec,
			wantSpec: builtinSpec, wantSource: sourceOnlyKeyed,
		},
		{
			name: "no keys and no terminal refuse with the credential error", keys: nil,
			origin: modelUnchosen, spec: builtinSpec,
			wantErr: provider.ErrCredentialNotSet, errHas: []string{"flynn auth set", "--model", "flynn models use"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keyedEnv(t, c.keys...)
			dir := t.TempDir()
			if c.saved != "" {
				if err := writeActiveModel(dir, c.saved); err != nil {
					t.Fatal(err)
				}
			}
			askWith(t, c.terminal, c.input)

			model, _, choice, err := resolveModelOrOnboard(context.Background(), c.spec, c.origin, dir)

			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				if model != nil {
					t.Error("a refusal returned a model")
				}
				for _, s := range c.errHas {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q does not mention %q", err, s)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("resolveModelOrOnboard: %v", err)
				}
				if choice.Spec != c.wantSpec || choice.Source != c.wantSource {
					t.Errorf("choice = %+v, want {%s %s}", choice, c.wantSpec, c.wantSource)
				}
			}
			got, _ := readActiveModel(dir)
			if got != c.wantSaved {
				t.Errorf("saved default = %q, want %q", got, c.wantSaved)
			}
		})
	}
}

func defaultModelOf(t *testing.T, name string) string {
	t.Helper()
	id, ok := provider.DefaultModelID(name)
	if !ok || id == "" {
		t.Fatalf("provider %s has no default model", name)
	}
	return id
}

// TestNothingChosenWithTwoKeysNeverRunsOnTheBuiltInSpec is the guard for the rule the
// resolver exists to keep: with no flag, no saved default and two providers keyed, the
// built-in spec is not used unless a person accepted it. Neither a missing terminal nor
// a terminal that goes quiet may fall through to it.
func TestNothingChosenWithTwoKeysNeverRunsOnTheBuiltInSpec(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
		input    string
	}{
		{"no terminal", false, ""},
		{"terminal that closes unanswered", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyedEnv(t, "anthropic", "openai")
			dir := t.TempDir()
			askWith(t, tc.terminal, tc.input)

			model, _, choice, err := resolveModelOrOnboard(context.Background(), builtinSpec, modelUnchosen, dir)

			if err == nil || model != nil || choice.Spec != "" {
				t.Fatalf("a run was resolved on %+v (err %v) though nothing was chosen and two providers are keyed", choice, err)
			}
			if got, ok := readActiveModel(dir); ok {
				t.Errorf("a default %q was saved without anyone choosing it", got)
			}
		})
	}
}

// TestKeyedAnswerSpec covers how a typed answer becomes a spec on a keyed provider.
func TestKeyedAnswerSpec(t *testing.T) {
	keyed := []string{"anthropic", "openai"}
	cases := []struct {
		answer  string
		want    string
		wantErr bool
	}{
		{"", "def:m", false},
		{"  ", "def:m", false},
		{"1", "anthropic:" + defaultModelOf(t, "anthropic"), false},
		{"openai", "openai:" + defaultModelOf(t, "openai"), false},
		{"openai:gpt-x", "openai:gpt-x", false},
		{"0", "", true},
		{"3", "", true},
		{"gemini", "", true},
		{"gemini:m", "", true},
	}
	for _, c := range cases {
		got, err := keyedAnswerSpec(c.answer, keyed, "def:m")
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("keyedAnswerSpec(%q) = %q, %v; want %q, error %v", c.answer, got, err, c.want, c.wantErr)
		}
		if err != nil && !errors.Is(err, errModelNotChosen) {
			t.Errorf("keyedAnswerSpec(%q) error %v does not wrap errModelNotChosen", c.answer, err)
		}
	}
}

// TestModelChoiceString is the text a run header prints.
func TestModelChoiceString(t *testing.T) {
	if got := (modelChoice{Spec: "openai:gpt-5.5", Source: sourceSaved}).String(); got != "openai:gpt-5.5 (saved default)" {
		t.Errorf("String = %q", got)
	}
	if got := (modelChoice{Spec: "openai:gpt-5.5"}).String(); got != "openai:gpt-5.5" {
		t.Errorf("String with no source = %q", got)
	}
}

// TestCurrentModelOrigin proves a flag outranks a saved default, and that the default
// being merely present in the flag set is not a choice.
func TestCurrentModelOrigin(t *testing.T) {
	priorE, priorS := modelSpecExplicit, modelSpecSaved
	t.Cleanup(func() { modelSpecExplicit, modelSpecSaved = priorE, priorS })
	for _, c := range []struct {
		explicit, saved bool
		want            modelOrigin
	}{
		{false, false, modelUnchosen},
		{false, true, modelFromSaved},
		{true, false, modelFromFlag},
		{true, true, modelFromFlag},
	} {
		modelSpecExplicit, modelSpecSaved = c.explicit, c.saved
		if got := currentModelOrigin(); got != c.want {
			t.Errorf("explicit=%v saved=%v: origin = %v, want %v", c.explicit, c.saved, got, c.want)
		}
	}
}

// TestRecordModelChoiceWritesTheResolvedModelToTheRunsStream proves the record carries
// the model and its source, and that a zero choice or a missing run writes nothing.
func TestRecordModelChoiceWritesTheResolvedModelToTheRunsStream(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	var out bytes.Buffer

	recordModelChoice(ctx, &out, log, modelChoice{}, "run-1")
	recordModelChoice(ctx, &out, log, modelChoice{Spec: "openai:gpt-5.5"}, "")
	if evs, _ := log.Read(ctx, spine.Query{Stream: "run-1"}); len(evs) != 0 {
		t.Fatalf("a zero choice or a missing run was recorded: %d events", len(evs))
	}

	recordModelChoice(ctx, &out, log, modelChoice{Spec: "openai:gpt-5.5", Source: sourceSaved}, "run-1")
	evs, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil || len(evs) != 1 || evs[0].Type != modelResolvedEventType {
		t.Fatalf("events = %v, %v; want one %s", evs, err, modelResolvedEventType)
	}
	if out.Len() != 0 {
		t.Fatalf("a successful record printed: %q", out.String())
	}
}
