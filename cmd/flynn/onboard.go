package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
func resolveModelOrOnboard(ctx context.Context, modelSpec, dataDir string) (llm.Model, harness.Plan, error) {
	model, plan, err := resolveModel(ctx, modelSpec, dataDir)
	if !errors.Is(err, provider.ErrCredentialNotSet) {
		return model, plan, err
	}

	// The requested provider has no key. If another provider is already configured,
	// use it rather than prompting; the default model spec just named a provider the
	// user has not set up.
	configured := configuredProviders(ctx, credentialSource(dataDir))
	switch {
	case len(configured) == 1:
		fmt.Fprintf(os.Stderr, "Using %s (already configured). Change it with `flynn models use <provider:model>`, or /model in a session.\n", configured[0])
		return resolveModel(ctx, configured[0], dataDir)
	case len(configured) > 1 && term.IsTerminal(int(os.Stdin.Fd())):
		in := bufio.NewReader(os.Stdin)
		fmt.Fprintf(os.Stderr, "Configured providers: %s\n", strings.Join(configured, ", "))
		name, perr := promptVisible(in, fmt.Sprintf("Provider [%s]: ", configured[0]))
		if perr != nil {
			return nil, harness.Plan{}, perr
		}
		if name == "" {
			name = configured[0]
		}
		return resolveModel(ctx, name, dataDir)
	case len(configured) > 1:
		// Non-interactive with several keys: pick deterministically rather than fail.
		return resolveModel(ctx, configured[0], dataDir)
	}

	// Nothing configured at all.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, harness.Plan{}, err
	}
	spec, oerr := onboardModel(ctx, modelSpec, dataDir)
	if oerr != nil {
		return nil, harness.Plan{}, oerr
	}
	return resolveModel(ctx, spec, dataDir)
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
// returns the model spec to resolve with.
func onboardModel(ctx context.Context, modelSpec, dataDir string) (string, error) {
	hosted := hostedCatalogModels()
	def := defaultOnboardSpec(modelSpec, hosted)

	fmt.Fprintln(os.Stderr, "Welcome to Flynn. No model is set up yet, so let's pick one.")
	for i, id := range hosted {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, id)
	}
	fmt.Fprintln(os.Stderr, "  (or type a provider:model, or a local model id from `flynn models`)")

	in := bufio.NewReader(os.Stdin)
	choice, err := promptVisible(in, fmt.Sprintf("Model [%s]: ", def))
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
	fmt.Fprintf(os.Stderr, "Using %s. Change it any time with `flynn models use <id>`, or /model in a session.\n\n", spec)
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

// promptVisible reads one echoed line from in and returns it trimmed. Unlike
// promptHidden it is for non-secret answers (a provider name), so the choice is
// visible as the user types it.
func promptVisible(in *bufio.Reader, label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}
