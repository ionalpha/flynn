package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// resolveModelOrOnboard resolves the model and, when the requested provider has no
// credential, recovers instead of failing with a raw error. It first prefers a
// provider whose key is already in the vault (so a stored key is used without
// asking), and only when none is configured and a terminal is attached does it run
// the first-run setup that stores a key. Without a terminal and with nothing
// configured, the original error is returned for the caller to surface.
//
// The recovery applies only to a spec the user did not choose (the built-in default
// or a saved one). When explicit is true the caller named the provider on the command
// line, so a missing credential is refused rather than answered by quietly sending the
// work, and the prompts in it, to a provider the caller did not ask for.
func resolveModelOrOnboard(ctx context.Context, modelSpec string, explicit bool, dataDir string) (llm.Model, harness.Plan, string, error) {
	model, plan, err := resolveModel(ctx, modelSpec, dataDir)
	if !errors.Is(err, provider.ErrCredentialNotSet) {
		// Resolved (or failed for another reason) with the requested spec, so that is
		// the model in use. Canonicalise it so a bare provider name reports its model.
		return model, plan, provider.CanonicalSpec(modelSpec), err
	}
	if explicit {
		name, _, _ := strings.Cut(modelSpec, ":")
		return nil, harness.Plan{}, "", fmt.Errorf("%w: %s has no credential; store one with `flynn auth set %s`, or name a configured provider with --model", err, name, name)
	}

	// The requested provider has no key. If another provider is already configured,
	// use it rather than prompting; the default model spec just named a provider the
	// user has not set up.
	configured := configuredProviders(ctx, credentialSource(dataDir))
	switch {
	case len(configured) == 1:
		fmt.Fprintf(os.Stderr, "Using %s (already configured). Change it with `flynn models use <provider:model>`, or /model in a session.\n", configured[0])
		return resolveResolvedSpec(ctx, configured[0], dataDir)
	case len(configured) > 1 && term.IsTerminal(int(os.Stdin.Fd())):
		in := bufio.NewReader(os.Stdin)
		fmt.Fprintf(os.Stderr, "Configured providers: %s\n", strings.Join(configured, ", "))
		name, perr := promptVisible(in, os.Stderr, fmt.Sprintf("Provider [%s]: ", configured[0]))
		if perr != nil {
			return nil, harness.Plan{}, "", perr
		}
		if name == "" {
			name = configured[0]
		}
		return resolveResolvedSpec(ctx, name, dataDir)
	case len(configured) > 1:
		// Non-interactive with several keys: pick deterministically rather than fail.
		return resolveResolvedSpec(ctx, configured[0], dataDir)
	}

	// Nothing configured at all.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, harness.Plan{}, "", err
	}
	spec, oerr := onboardModel(ctx, os.Stdin, os.Stderr, modelSpec, dataDir)
	if oerr != nil {
		return nil, harness.Plan{}, "", oerr
	}
	return resolveResolvedSpec(ctx, spec, dataDir)
}

// resolveResolvedSpec resolves spec and reports the canonical model id it resolved
// to alongside the model, so a caller records the concrete model actually in use
// (a bare provider name becomes "<provider>:<default model>") rather than whatever
// default spec started the resolution.
func resolveResolvedSpec(ctx context.Context, spec, dataDir string) (llm.Model, harness.Plan, string, error) {
	model, plan, err := resolveModel(ctx, spec, dataDir)
	return model, plan, provider.CanonicalSpec(spec), err
}

// configuredProviders returns the known providers that already have a credential in
// src, in the canonical provider order, so a caller can prefer a set-up provider
// over prompting for a new one.
func configuredProviders(ctx context.Context, src secret.Source) []string {
	var out []string
	for _, name := range provider.Providers() {
		ref, ok := provider.KeyRef(name)
		if !ok {
			continue
		}
		if _, err := src.Lookup(ctx, ref); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// keyedProviders lists the providers that take an API key, in canonical order, so
// the credential prompt offers only providers there is actually a key to enter for.
func keyedProviders() []string {
	var out []string
	for _, name := range provider.Providers() {
		if _, ok := provider.KeyRef(name); ok {
			out = append(out, name)
		}
	}
	return out
}

// onboardModel is the first-run setup: it shows the hosted models from the catalog,
// lets the user pick one (by number, or by typing any provider:model or local model id),
// stores the API key for the chosen provider when one is needed and not already set, and
// records the choice as the default so a later launch reuses it without --model. It
// returns the model spec to resolve with. The pick list is printed to out and the
// answer read from in, so the exchange can be driven over any pair of streams.
func onboardModel(ctx context.Context, in io.Reader, out io.Writer, modelSpec, dataDir string) (string, error) {
	hosted := hostedCatalogModels()
	def := defaultOnboardSpec(modelSpec, hosted)

	_, _ = fmt.Fprintln(out, "Welcome to Flynn. No model is set up yet, so let's pick one.")
	for i, id := range hosted {
		_, _ = fmt.Fprintf(out, "  %d) %s\n", i+1, id)
	}
	_, _ = fmt.Fprintln(out, "  (or type a provider:model, or a local model id from `flynn models`)")

	r := bufio.NewReader(in)
	choice, err := promptVisible(r, out, fmt.Sprintf("Model [%s]: ", def))
	if err != nil {
		return "", err
	}
	spec := onboardChoiceSpec(choice, hosted, def)
	if spec == "" {
		return "", errors.New("no model chosen")
	}

	// If the chosen model's provider needs an API key and none is stored yet, ask for it
	// and seal it. A local model or a keyless provider skips this.
	if err := ensureProviderKey(ctx, spec, dataDir); err != nil {
		return "", err
	}

	if err := writeActiveModel(dataDir, spec); err != nil {
		return "", fmt.Errorf("record model selection: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Using %s. Change it any time with `flynn models use <id>`, or /model in a session.\n\n", spec)
	return spec, nil
}

// hostedCatalogModels lists the hosted (API) models from the catalog as provider:model
// ids, the first-run pick list. If the catalog cannot be read, it falls back to the
// default model of each keyed provider, so onboarding still offers a usable choice.
func hostedCatalogModels() []string {
	cat, err := catalog.Load()
	if err == nil {
		var ids []string
		for _, m := range cat.Find(catalog.Query{Kind: catalog.KindAPI}) {
			ids = append(ids, m.ID)
		}
		if len(ids) > 0 {
			return ids
		}
	}
	return keyedProviders()
}

// defaultOnboardSpec picks the pre-filled onboarding choice: the requested spec when it
// names a keyed provider, else the first hosted model, else the requested spec as typed.
func defaultOnboardSpec(modelSpec string, hosted []string) string {
	if name, _, ok := strings.Cut(modelSpec, ":"); ok {
		if _, keyed := provider.KeyRef(name); keyed {
			return modelSpec
		}
	}
	if len(hosted) > 0 {
		return hosted[0]
	}
	return modelSpec
}

// onboardChoiceSpec turns the user's answer into a model spec: an empty answer takes the
// default, a number selects from the offered list, and anything else is treated as a spec
// typed directly (a provider:model or a local model id).
func onboardChoiceSpec(choice string, hosted []string, def string) string {
	choice = strings.TrimSpace(choice)
	if choice == "" {
		return def
	}
	if n, err := strconv.Atoi(choice); err == nil {
		if n >= 1 && n <= len(hosted) {
			return hosted[n-1]
		}
		return "" // an out-of-range number is a mistake, not a spec
	}
	return choice
}

// ensureProviderKey stores an API key for spec's provider when the provider takes one and
// none is set yet, reading it without echo and sealing it in the vault. A local model or
// a keyless provider needs nothing and returns nil.
func ensureProviderKey(ctx context.Context, spec, dataDir string) error {
	name, _, _ := strings.Cut(spec, ":")
	ref, ok := provider.KeyRef(name)
	if !ok {
		return nil // keyless (a local server) or not a provider spec
	}
	if _, err := credentialSource(dataDir).Lookup(ctx, ref); err == nil {
		return nil // already have a key for this provider
	}

	key, err := promptHidden(fmt.Sprintf("Enter API key for %s: ", name))
	if err != nil {
		return err
	}
	if key.Empty() {
		return errors.New("no key entered")
	}
	store := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))
	if err := store.Set(ctx, ref, key); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Stored %s in the vault (encrypted at rest, revealed only to call the model).\n", ref)
	return nil
}

// promptVisible writes label to out, reads one echoed line from in, and returns it
// trimmed. Unlike promptHidden it is for non-secret answers (a provider name), so the
// choice is visible as the user types it.
func promptVisible(in *bufio.Reader, out io.Writer, label string) (string, error) {
	_, _ = fmt.Fprint(out, label)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}
