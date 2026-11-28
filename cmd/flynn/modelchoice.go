package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/spine"
)

// modelOrigin says how the model spec reached the resolver. Only a person's act is a
// choice: the --model flag or a saved default. The flag's built-in default is
// [modelUnchosen], a suggestion that never decides which provider sees the work.
type modelOrigin int

const (
	modelUnchosen modelOrigin = iota
	modelFromFlag
	modelFromSaved
)

// modelSpecSaved records that the spec in use came from the saved default. Like
// [modelSpecExplicit] it is set once per invocation, because every command that
// resolves a model has to agree on whether anyone chose it.
var modelSpecSaved bool

// currentModelOrigin is the origin of this invocation's model spec.
func currentModelOrigin() modelOrigin {
	switch {
	case modelSpecExplicit:
		return modelFromFlag
	case modelSpecSaved:
		return modelFromSaved
	}
	return modelUnchosen
}

// The sources a run reports beside its model.
const (
	sourceFlag        = "--model"
	sourceSaved       = "saved default"
	sourcePrompt      = "chosen at the prompt"
	sourceSetup       = "first-run setup"
	sourceOnlyKeyed   = "the only provider with a key"
	savedAsDefaultTag = ", saved as default"
)

// modelChoice is the model a run resolved to and where that came from, so a run on a
// model nobody typed is visible in its header and its record.
type modelChoice struct {
	Spec   string
	Source string
}

// String renders the choice the way a run header states it.
func (c modelChoice) String() string {
	if c.Source == "" {
		return c.Spec
	}
	return c.Spec + " (" + c.Source + ")"
}

// errModelNotChosen is wrapped by every refusal to pick a model on the operator's
// behalf, so a caller (and a test) can tell it from a credential or network failure.
var errModelNotChosen = errors.New("no model chosen")

// modelAsk is where the resolver asks its questions. It is a value so a test supplies a
// terminal and its answers without a pty.
type modelAsk struct {
	terminal bool
	in       io.Reader
	out      io.Writer
}

// modelAskIO returns the real streams. Tests replace it.
var modelAskIO = func() modelAsk {
	return modelAsk{terminal: term.IsTerminal(int(os.Stdin.Fd())), in: os.Stdin, out: os.Stderr}
}

// resolveModelOrOnboard resolves the model a run uses and reports where the choice came
// from. A spec the operator chose (--model or a saved default) is used as named, and a
// missing credential is refused: the work and its prompts never go to a provider the
// operator did not pick.
//
// With nothing chosen, the built-in spec is only a suggestion. Several providers with a
// key: ask which and save the answer, or refuse when nobody can be asked. Exactly one:
// that provider is the only place the work can go, so it is used (after one question on
// a terminal, which also saves the answer). None: the first-run setup on a terminal,
// else a refusal that says how to store a key.
func resolveModelOrOnboard(ctx context.Context, modelSpec string, origin modelOrigin, dataDir string) (llm.Model, harness.Plan, modelChoice, error) {
	if origin != modelUnchosen {
		source := sourceFlag
		if origin == modelFromSaved {
			source = sourceSaved
		}
		model, plan, err := resolveModel(ctx, modelSpec, dataDir)
		if errors.Is(err, provider.ErrCredentialNotSet) {
			name, _, _ := strings.Cut(modelSpec, ":")
			return nil, harness.Plan{}, modelChoice{}, fmt.Errorf("%w: %s has no credential (%s); store one with `flynn auth set %s`, or choose a configured provider with --model or `flynn models use`", err, name, source, name)
		}
		return model, plan, modelChoice{Spec: provider.CanonicalSpec(modelSpec), Source: source}, err
	}

	chosen, err := chooseModel(ctx, modelSpec, dataDir, modelAskIO())
	if err != nil {
		return nil, harness.Plan{}, modelChoice{}, err
	}
	model, plan, err := resolveModel(ctx, chosen.Spec, dataDir)
	return model, plan, chosen, err
}

// chooseModel settles a run whose operator named no model. It never returns the
// built-in spec merely because it is the default: the spec comes back only when the
// operator accepted it at a prompt, or it names the one provider with a key.
func chooseModel(ctx context.Context, builtin, dataDir string, ask modelAsk) (modelChoice, error) {
	keyed := configuredProviders(ctx, credentialSource(dataDir))
	switch len(keyed) {
	case 0:
		if !ask.terminal {
			return modelChoice{}, fmt.Errorf("%w: no model is chosen and no provider has a credential; store a key with `flynn auth set <provider>`, then choose a model with --model or `flynn models use <provider:model>`: %w", provider.ErrCredentialNotSet, errModelNotChosen)
		}
		spec, err := onboardModel(ctx, ask.in, ask.out, builtin, dataDir)
		if err != nil {
			return modelChoice{}, err
		}
		return modelChoice{Spec: provider.CanonicalSpec(spec), Source: sourceSetup + savedAsDefaultTag}, nil
	case 1:
		return chooseOnlyKeyed(builtin, dataDir, keyed[0], ask)
	}
	if !ask.terminal {
		return modelChoice{}, fmt.Errorf("%w: several providers have a key (%s) and none is chosen; name one with --model <provider:model>, or save a default with `flynn models use <provider:model>`", errModelNotChosen, strings.Join(keyed, ", "))
	}
	return chooseAmongKeyed(builtin, dataDir, keyed, ask)
}

// suggestedSpec is the built-in spec when its provider has a key, else that of the
// first provider that does. It only fills the prompt's default and the one-key case.
func suggestedSpec(builtin string, keyed []string) string {
	if name, _, _ := strings.Cut(builtin, ":"); slices.Contains(keyed, name) {
		return builtin
	}
	return provider.CanonicalSpec(keyed[0])
}

// chooseOnlyKeyed handles exactly one provider with a key. Nothing else could receive
// the work or the spend, so a run proceeds on it; on a terminal the operator is asked
// once so the pick is recorded as theirs and never asked again, because an automatic save
// would turn an inference into a stored choice that outlives the second key. Without a
// terminal nothing is saved and the run says on stderr which provider it took.
func chooseOnlyKeyed(builtin, dataDir, only string, ask modelAsk) (modelChoice, error) {
	spec := suggestedSpec(builtin, []string{only})
	if !ask.terminal {
		_, _ = fmt.Fprintf(ask.out, "No model is chosen; using %s, the only provider with a key. Choose one with --model, or save a default with `flynn models use <provider:model>`.\n", spec)
		return modelChoice{Spec: provider.CanonicalSpec(spec), Source: sourceOnlyKeyed}, nil
	}
	answer, err := promptVisible(bufio.NewReader(ask.in), ask.out, fmt.Sprintf("No model is chosen and only %s has a key. Use %s and save it as your default? [Y/n]: ", only, spec))
	if err != nil {
		return modelChoice{}, err
	}
	if a := strings.ToLower(answer); a != "" && a != "y" && a != "yes" {
		return modelChoice{}, fmt.Errorf("%w: choose one with --model <provider:model>, or save a default with `flynn models use <provider:model>`", errModelNotChosen)
	}
	return saveChoice(dataDir, spec, sourceOnlyKeyed, ask.out), nil
}

// chooseAmongKeyed asks which of several keyed providers to use, offering the built-in
// spec as the default answer when its provider is among them, and saves the answer.
func chooseAmongKeyed(builtin, dataDir string, keyed []string, ask modelAsk) (modelChoice, error) {
	def := suggestedSpec(builtin, keyed)
	_, _ = fmt.Fprintln(ask.out, "No model is chosen and several providers have a key:")
	for i, name := range keyed {
		_, _ = fmt.Fprintf(ask.out, "  %d) %s\n", i+1, name)
	}
	answer, err := promptVisible(bufio.NewReader(ask.in), ask.out, fmt.Sprintf("Pick one (number, provider, or provider:model) [%s]: ", def))
	if err != nil {
		return modelChoice{}, err
	}
	spec, err := keyedAnswerSpec(answer, keyed, def)
	if err != nil {
		return modelChoice{}, err
	}
	return saveChoice(dataDir, spec, sourcePrompt, ask.out), nil
}

// keyedAnswerSpec turns an answer into a spec on one of the keyed providers: empty takes
// def, a number picks from the list, a bare provider takes its default model.
func keyedAnswerSpec(answer string, keyed []string, def string) (string, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(answer); err == nil {
		if n < 1 || n > len(keyed) {
			return "", fmt.Errorf("%w: %d is not one of the choices 1 to %d", errModelNotChosen, n, len(keyed))
		}
		return provider.CanonicalSpec(keyed[n-1]), nil
	}
	if name, _, _ := strings.Cut(answer, ":"); !slices.Contains(keyed, name) {
		return "", fmt.Errorf("%w: %q is not a provider with a key (%s)", errModelNotChosen, name, strings.Join(keyed, ", "))
	}
	return provider.CanonicalSpec(answer), nil
}

// saveChoice records spec as the saved default, the same file `flynn models use` writes,
// so there is one place a default lives. A failed write does not stop the run, which was
// already decided; the source says it was not saved.
func saveChoice(dataDir, spec, source string, out io.Writer) modelChoice {
	if err := writeActiveModel(dataDir, spec); err != nil {
		_, _ = fmt.Fprintf(out, "Could not save %s as your default (%v); it applies to this run only.\n", spec, err)
		return modelChoice{Spec: spec, Source: source}
	}
	_, _ = fmt.Fprintf(out, "Saved %s as your default. Change it with `flynn models use <provider:model>`, or /model in a session.\n", spec)
	return modelChoice{Spec: spec, Source: source + savedAsDefaultTag}
}

// modelResolvedEventType is the stream event that names the model a run resolved to and
// where the choice came from.
const modelResolvedEventType = "model.resolved"

// recordModelChoice writes the resolved model and its source to the run's stream. A zero
// choice is not recorded. A failure is reported and does not stop the run, which already
// resolved; the missing record is what is worth saying.
func recordModelChoice(ctx context.Context, out io.Writer, log spine.Log, m modelChoice, runID string) {
	if m.Spec == "" || runID == "" || log == nil {
		return
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream:  runID,
		Type:    modelResolvedEventType,
		Actor:   spine.ActorSystem,
		Payload: map[string]any{"model": m.Spec, "source": m.Source},
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "  (model choice not recorded: %v)\n", err)
	}
}
